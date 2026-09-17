# Client Presence WebSocket (API side) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `GET /v2/client/ws`, a persistent WebSocket the EMLy Updater holds open for real-time online/offline presence and an envelope open to future server→client commands, and surface that presence on `GET /v2/stats/clients` and the `stats:clients` channel of `GET /v2/stats/stream`.

**Architecture:** A new in-memory, HTTP- and DB-free `internal/presencehub` package (same shape as `internal/statshub`) tracks which `updater_clients.id` currently holds an open connection, with a short grace period on disconnect. A new handler (`internal/handlers/client_ws.route.go`) accepts the upgrade, exchanges a one-message handshake (`hello` → `identity`) that reuses the existing `clientIdentity`/`upsertUpdaterClient` machinery from `updates.route.go`, then runs a 10s server-ping heartbeat. The route is mounted like `/v2/stats/stream` already is: behind `apimw.APIKeyAuth`, and bypassing chi's `Timeout` middleware in `main.go` because `coder/websocket.Accept` needs `http.Hijacker`.

**Tech Stack:** Go, `github.com/coder/websocket`, `github.com/jmoiron/sqlx`, `github.com/go-chi/chi/v5`. No new external dependency.

**Spec:** `docs/superpowers/specs/2026-09-17-client-presence-ws-api-design.md` (this repo, source of truth for the protocol) and its companion `emly-updater/docs/superpowers/specs/2026-09-17-client-presence-ws-design.md` (client side, out of scope for this plan).

## Global Constraints

- Auth on the upgrade is `X-Api-Key` via `apimw.APIKeyAuth`, no query-string fallback (unlike `/v2/stats/stream`'s admin-key check).
- Presence state is in-memory only (`internal/presencehub`) — no new DB column, no new table.
- Heartbeat: server pings every 10s; a connection that goes 20s without any frame from the client is considered dead.
- Disconnect grace period: 15s before a client is actually marked offline, so a brief drop/reconnect never flickers on the dashboard.
- Identity JSON carries the same field set as today's `X-EMLy-*` headers: `hwid`, `hostname`, `ad_domain`, `logged_user`, `logged_user_state`, `logged_user_disconnected_at`, `serial`, `product`, `os_version`, `emly_version`. `updater_version`/`contact` come from the upgrade request's User-Agent, `ip` from the connection — never from the JSON payload.
- The envelope's `type` field must stay open: an unrecognized `type` is logged and ignored, never a reason to close the connection.
- Kill switch: `Document.ClientWS *ToggleOnly` (`json:"clientWs"`) in `internal/remoteconfig`, reusing the existing `ToggleOnly{Enabled bool}` type — default absent/nil, which the client side treats as disabled. No enforcement needed on the API side beyond serving the field; the API does not reject a connection because `clientWs.enabled` is false (that's the client's own gate).
- No new environment variables.
- `GET /v2/client/ws` must be mounted in `main.go`'s hijack-safe `mux`, exactly like `/v2/stats/stream`, never left in the `chiMiddleware.Timeout`-wrapped `r` alone.
- `online` is a response-time decoration (`models.UpdaterClient.Online`, `db:"-"`), computed from `presencehub.Hub.Online(id)`, added only to `GET /v2/stats/clients` and the `stats:clients` WS channel — not `GET /v2/stats/clients/{id}` (out of this spec's scope).
- `CLAUDE.md` and `ROUTES.md` must be updated in the same change per this repo's documentation-upkeep rule (new package, new route group, new auth-relevant behavior).

---

### Task 1: `internal/presencehub` — in-memory presence tracking

**Files:**
- Create: `internal/presencehub/presencehub.go`
- Create: `internal/presencehub/presencehub_test.go`

**Interfaces:**
- Produces: `presencehub.New(graceDuration time.Duration) *Hub`, `presencehub.DefaultGraceDuration` (`time.Duration`, `15 * time.Second`), `(*Hub).Connect(clientID int64) (Token, <-chan struct{})`, `(*Hub).Disconnect(Token)`, `(*Hub).Online(clientID int64) bool` (nil-safe: a nil `*Hub` always reports `false`).

- [ ] **Step 1: Write the failing tests**

```go
// internal/presencehub/presencehub_test.go
package presencehub

import (
	"testing"
	"time"
)

const testGrace = 30 * time.Millisecond

func TestConnectMarksOnline(t *testing.T) {
	h := New(testGrace)
	if h.Online(1) {
		t.Fatal("Online(1) = true before any Connect")
	}
	tok, _ := h.Connect(1)
	if !h.Online(1) {
		t.Fatal("Online(1) = false right after Connect")
	}
	h.Disconnect(tok)
}

func TestDisconnectStaysOnlineDuringGrace(t *testing.T) {
	h := New(testGrace)
	tok, _ := h.Connect(1)
	h.Disconnect(tok)
	if !h.Online(1) {
		t.Fatal("Online(1) = false immediately after Disconnect, want true during grace period")
	}
}

func TestDisconnectGoesOfflineAfterGraceExpires(t *testing.T) {
	h := New(testGrace)
	tok, _ := h.Connect(1)
	h.Disconnect(tok)
	time.Sleep(testGrace * 3)
	if h.Online(1) {
		t.Fatal("Online(1) = true after grace period expired, want false")
	}
}

func TestReconnectDuringGraceCancelsIt(t *testing.T) {
	h := New(testGrace)
	tok1, _ := h.Connect(1)
	h.Disconnect(tok1)

	tok2, _ := h.Connect(1)
	time.Sleep(testGrace * 3)
	if !h.Online(1) {
		t.Fatal("Online(1) = false after reconnecting during grace, want true")
	}
	h.Disconnect(tok2)
}

func TestNewConnectionSupersedesOld(t *testing.T) {
	h := New(testGrace)
	_, supersede1 := h.Connect(1)

	select {
	case <-supersede1:
		t.Fatal("supersede channel closed before a second Connect")
	default:
	}

	h.Connect(1)

	select {
	case <-supersede1:
		// expected: the first connection is told to close.
	case <-time.After(time.Second):
		t.Fatal("supersede channel never closed after a second Connect for the same clientID")
	}
}

func TestStaleDisconnectIsNoop(t *testing.T) {
	h := New(testGrace)
	tok1, _ := h.Connect(1)
	h.Connect(1) // supersedes tok1

	h.Disconnect(tok1) // stale: must not touch the new connection's entry
	if !h.Online(1) {
		t.Fatal("Online(1) = false after a stale Disconnect from a superseded connection")
	}
}

func TestOnlineOnNilHub(t *testing.T) {
	var h *Hub
	if h.Online(1) {
		t.Fatal("Online(1) on a nil *Hub = true, want false")
	}
}

func TestDifferentClientsAreIndependent(t *testing.T) {
	h := New(testGrace)
	tok1, _ := h.Connect(1)
	h.Connect(2)

	if !h.Online(1) || !h.Online(2) {
		t.Fatal("both clients should be online")
	}
	h.Disconnect(tok1)
	time.Sleep(testGrace * 3)
	if h.Online(1) {
		t.Fatal("client 1 should be offline after its own grace period")
	}
	if !h.Online(2) {
		t.Fatal("client 2 must stay online, unaffected by client 1's disconnect")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/presencehub/... -v`
Expected: FAIL (build error — package `presencehub` does not exist yet)

- [ ] **Step 3: Write the implementation**

```go
// internal/presencehub/presencehub.go

// Package presencehub tracks which EMLy Updater clients (keyed by
// updater_clients.id) currently hold an open GET /v2/client/ws connection,
// in memory only - HTTP- and DB-free, like internal/statshub, and subject to
// the same single-instance limit (see that package's doc): presence known to
// one API replica is invisible to another. internal/handlers is its only
// caller, for decorating GET /v2/stats/clients and the stats:clients WS
// channel with an "online" field (2026-09-17-client-presence-ws-api-design.md
// §5).
package presencehub

import (
	"sync"
	"time"
)

// DefaultGraceDuration is how long a client stays "online" after its
// connection drops before it is actually marked offline (design doc §5): it
// absorbs a brief reconnect - a network blip, or a newer connection
// superseding an older one - without a flicker on the dashboard.
const DefaultGraceDuration = 15 * time.Second

// Token identifies one Connect call. Its matching Disconnect only acts on
// the entry it created: a superseded connection's own (deferred) Disconnect
// must be a no-op on the newer connection that replaced it, not undo its
// registration.
type Token struct {
	clientID int64
	gen      uint64
}

type entry struct {
	gen        uint64
	supersede  chan struct{}
	graceTimer *time.Timer
}

// Hub tracks online presence per updater_clients.id. The zero value is not
// usable; construct with New. Safe for concurrent use.
type Hub struct {
	mu      sync.Mutex
	grace   time.Duration
	entries map[int64]*entry
	nextGen uint64
}

// New returns an empty, ready-to-use Hub. graceDuration is normally
// DefaultGraceDuration; tests pass a shorter one so they don't have to sleep
// for 15 real seconds.
func New(graceDuration time.Duration) *Hub {
	return &Hub{grace: graceDuration, entries: make(map[int64]*entry)}
}

// Connect registers clientID as online for a new connection. It returns a
// Token (pass to Disconnect when this connection ends) and a channel that
// closes if a *newer* Connect call for the same clientID supersedes this
// one - the caller's connection loop should select on it and close its own
// socket (design doc §5: "sostituisce quella vecchia... la connessione
// precedente viene chiusa lato server"). Connecting also cancels any pending
// grace-period removal left over from a previous Disconnect for this
// clientID, so a reconnect inside the grace window is seamless.
func (h *Hub) Connect(clientID int64) (Token, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if old, ok := h.entries[clientID]; ok {
		if old.graceTimer != nil {
			old.graceTimer.Stop()
		}
		close(old.supersede)
	}

	h.nextGen++
	gen := h.nextGen
	e := &entry{gen: gen, supersede: make(chan struct{})}
	h.entries[clientID] = e
	return Token{clientID: clientID, gen: gen}, e.supersede
}

// Disconnect ends one Connect'd connection. Rather than marking the client
// offline immediately, it arms a grace-period timer that removes the entry
// when it fires; a fresh Connect for the same clientID before then cancels
// it. A Disconnect whose Token no longer matches the current entry (the
// connection was superseded - see Connect) is a no-op, so cleanup from the
// old connection never touches the new one's state.
func (h *Hub) Disconnect(tok Token) {
	h.mu.Lock()
	defer h.mu.Unlock()

	e, ok := h.entries[tok.clientID]
	if !ok || e.gen != tok.gen {
		return
	}

	clientID, gen := tok.clientID, tok.gen
	e.graceTimer = time.AfterFunc(h.grace, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if cur, ok := h.entries[clientID]; ok && cur.gen == gen {
			delete(h.entries, clientID)
		}
	})
}

// Online reports whether clientID currently has an open connection, or is
// still inside the grace window after one dropped. A nil Hub reports every
// client offline, so a caller that never wired presence tracking (tests, a
// build that never constructs one) doesn't need to special-case it.
func (h *Hub) Online(clientID int64) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.entries[clientID]
	return ok
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/presencehub/... -v`
Expected: PASS (all 8 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/presencehub/presencehub.go internal/presencehub/presencehub_test.go
git commit -m "feat: add internal/presencehub for client WS online tracking"
```

---

### Task 2: Remote-config kill switch — `Document.ClientWS`

**Files:**
- Modify: `internal/remoteconfig/types.go:44-51`
- Modify: `testdata/remoteconfig/valid/full.json`
- Test: `internal/remoteconfig/parse_test.go` (add a case; the file already table-drives fixtures)

**Interfaces:**
- Consumes: existing `ToggleOnly{Enabled bool}` type (`internal/remoteconfig/types.go:148-150`).
- Produces: `Document.ClientWS *ToggleOnly` (`json:"clientWs"`), automatically covered by `Parse`, `Canonical` and `UnknownFieldPaths` (all three operate reflectively/structurally on `Document`, no separate registration needed).

- [ ] **Step 1: Write the failing test**

```go
// internal/remoteconfig/parse_test.go — add near the other top-level field tests
func TestParse_ClientWSDefaultsToNilWhenAbsent(t *testing.T) {
	doc, problems := Parse([]byte(`{
		"schemaVersion": 1,
		"revision": 1,
		"generatedAt": "",
		"servers": {"a": "https://a.example.com"},
		"defaultServer": "a"
	}`))
	if len(problems) != 0 {
		t.Fatalf("expected valid, got problems: %+v", problems)
	}
	if doc.ClientWS != nil {
		t.Fatalf("ClientWS = %+v, want nil when the document omits clientWs", doc.ClientWS)
	}
}

func TestParse_ClientWSEnabled(t *testing.T) {
	doc, problems := Parse([]byte(`{
		"schemaVersion": 1,
		"revision": 1,
		"generatedAt": "",
		"servers": {"a": "https://a.example.com"},
		"defaultServer": "a",
		"clientWs": {"enabled": true}
	}`))
	if len(problems) != 0 {
		t.Fatalf("expected valid, got problems: %+v", problems)
	}
	if doc.ClientWS == nil || !doc.ClientWS.Enabled {
		t.Fatalf("ClientWS = %+v, want {Enabled: true}", doc.ClientWS)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/remoteconfig/... -run TestParse_ClientWS -v`
Expected: FAIL — `doc.ClientWS` is not a field on `Document` (build error)

- [ ] **Step 3: Add the field**

```go
// internal/remoteconfig/types.go — inside type Document struct, replacing lines 46-51:
	Updater *UpdaterTuning `json:"updater"`

	Logging *Logging `json:"logging"`

	// ClientWS is the kill switch for GET /v2/client/ws (design doc
	// 2026-09-17-client-presence-ws-api-design.md §8.2), reusing the same
	// {enabled} shape as SelfUpdate/InstallCertificate. Absent/nil means the
	// client keeps the channel closed - updating the updater alone must
	// never open it.
	ClientWS *ToggleOnly `json:"clientWs"`

	Overrides []Override `json:"overrides"`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/remoteconfig/... -run TestParse_ClientWS -v`
Expected: PASS

- [ ] **Step 5: Add `clientWs` to the `full` fixture and re-run the whole fixture suite**

```json
// testdata/remoteconfig/valid/full.json — add after the "logging" block, before "overrides":
  "logging": {
    "level": "info",
    "maxSizeMB": 2,
    "backups": 5,
    "compress": true,
    "eventLog": true
  },
  "clientWs": { "enabled": true },
  "overrides": [
```

Run: `go test ./internal/remoteconfig/... -v`
Expected: PASS (including `TestFixtures_Valid`, which now also parses the updated `full.json`)

- [ ] **Step 6: Commit**

```bash
git add internal/remoteconfig/types.go internal/remoteconfig/parse_test.go testdata/remoteconfig/valid/full.json
git commit -m "feat: add clientWs kill switch to the remote config document"
```

Note (cross-repo, not part of this plan): `emly-updater/internal/policy` needs the mirror-image field and its own copy of `testdata/remoteconfig/valid/full.json` updated identically — tracked by the updater-side plan.

---

### Task 3: `models.UpdaterClient.Online`

**Files:**
- Modify: `internal/models/updater_stat.go:42-44`

**Interfaces:**
- Produces: `UpdaterClient.Online bool` (`db:"-"`, `json:"online"`), always `false` unless a caller explicitly sets it (see Task 6/7's `decorateOnline`).

- [ ] **Step 1: Add the field**

```go
// internal/models/updater_stat.go — replace lines 42-44:
	FirstSeenAt     time.Time  `db:"first_seen_at"     json:"first_seen_at"`
	LastSeenAt      time.Time  `db:"last_seen_at"      json:"last_seen_at"`
	// Online reports whether this client currently holds an open
	// GET /v2/client/ws connection (internal/presencehub), or is inside the
	// short grace window after one dropped. It is computed at response time
	// from the in-memory presence hub, never stored - db:"-" keeps sqlx's
	// `SELECT *` from trying to bind a non-existent column - so it is false
	// on any row nobody has explicitly decorated (see decorateOnline in
	// internal/handlers/stats.route.go).
	Online bool `db:"-" json:"online"`
}
```

- [ ] **Step 2: Verify the package still builds and existing tests still pass**

Run: `go build ./... && go test ./internal/models/... ./internal/handlers/... -v`
Expected: PASS — a `db:"-"` field must not break any existing `SELECT * FROM updater_clients` scan.

- [ ] **Step 3: Commit**

```bash
git add internal/models/updater_stat.go
git commit -m "feat: add Online field to UpdaterClient"
```

---

### Task 4: Identity payload conversion — `clientIdentityFromWSPayload`

**Files:**
- Create: `internal/handlers/client_ws.route.go` (this task only adds the types + the pure conversion function; Task 5 adds the connection handler to the same file)
- Test: `internal/handlers/client_ws_test.go`

**Interfaces:**
- Consumes: `clientIdentity` (`internal/handlers/updates.route.go:66-94`), `parseUpdaterUserAgent`, `parseLoggedUserSession`, `truncate`, `reportsNobodyLoggedOn`, `clientIPFromRequest` (all in `internal/handlers/updates.route.go`, same package).
- Produces: `clientWSInMessage{Type string; Data json.RawMessage}`, `clientWSIdentityPayload{HWID, Hostname, ADDomain, LoggedUser, LoggedUserState, LoggedUserDisconnectedAt, Serial, Product, OSVersion, EMLyVersion string}`, `clientIdentityFromWSPayload(r *http.Request, p clientWSIdentityPayload) clientIdentity`.

- [ ] **Step 1: Write the failing test**

```go
// internal/handlers/client_ws_test.go
package handlers

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIdentityFromWSPayload(t *testing.T) {
	r := httptest.NewRequest("GET", "/v2/client/ws", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("User-Agent", "EMLy-Updater/1.6.3 (f.fois@3git.eu)")

	payload := clientWSIdentityPayload{
		HWID:                     "36CC511A-F0DE-EA11-8106-842AFDCE34D0",
		Hostname:                 "PC-01",
		ADDomain:                 "contoso.local",
		LoggedUser:               `CONTOSO\mario.rossi`,
		LoggedUserState:          "disconnected",
		LoggedUserDisconnectedAt: "2026-09-12T18:04:31Z",
		Serial:                   "CND0342SLW",
		Product:                  "1F3N0EA#ABZ",
		OSVersion:                "Windows 11 24H2 Professional (Build 26100.4652)",
		EMLyVersion:              "3.4.1",
	}

	got := clientIdentityFromWSPayload(r, payload)
	want := clientIdentity{
		HWID:                     "36CC511A-F0DE-EA11-8106-842AFDCE34D0",
		Hostname:                 "PC-01",
		ADDomain:                 "contoso.local",
		LoggedUser:               `CONTOSO\mario.rossi`,
		LoggedUserState:          "disconnected",
		LoggedUserDisconnectedAt: time.Date(2026, 9, 12, 18, 4, 31, 0, time.UTC),
		Serial:                   "CND0342SLW",
		Product:                  "1F3N0EA#ABZ",
		OSVersion:                "Windows 11 24H2 Professional (Build 26100.4652)",
		EMLyVersion:              "3.4.1",
		UAVersion:                "1.6.3",
		Contact:                  "f.fois@3git.eu",
		IP:                       "10.0.0.5",
	}
	if got != want {
		t.Fatalf("clientIdentityFromWSPayload =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestClientIdentityFromWSPayloadUnidentified(t *testing.T) {
	r := httptest.NewRequest("GET", "/v2/client/ws", nil)
	got := clientIdentityFromWSPayload(r, clientWSIdentityPayload{})
	if got.identified() {
		t.Fatalf("an empty payload must not be identified()")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/handlers/... -run TestClientIdentityFromWSPayload -v`
Expected: FAIL (build error — `clientWSIdentityPayload`/`clientIdentityFromWSPayload` don't exist)

- [ ] **Step 3: Write the implementation**

```go
// internal/handlers/client_ws.route.go
package handlers

import (
	"encoding/json"
	"net/http"
)

// clientWSInMessage is the client->server envelope for GET /v2/client/ws:
// only "identity" and "pong" are meaningful today (design doc §3), but the
// shape stays generic so a future message type doesn't need a new envelope.
type clientWSInMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// clientWSIdentityPayload is the "identity" message's data (design doc
// §3.1): the same set of facts the EMLy Updater already sends as X-EMLy-*
// headers on every manifest/download request, carried in JSON here instead.
// Every field is optional exactly like its header counterpart - absent means
// "not reported", not "empty" (see clientIdentityFromWSPayload).
type clientWSIdentityPayload struct {
	HWID                     string `json:"hwid"`
	Hostname                 string `json:"hostname"`
	ADDomain                 string `json:"ad_domain"`
	LoggedUser               string `json:"logged_user"`
	LoggedUserState          string `json:"logged_user_state"`
	LoggedUserDisconnectedAt string `json:"logged_user_disconnected_at"`
	Serial                   string `json:"serial"`
	Product                  string `json:"product"`
	OSVersion                string `json:"os_version"`
	EMLyVersion              string `json:"emly_version"`
}

// clientIdentityFromWSPayload builds a clientIdentity from an "identity"
// message, the WS counterpart of clientIdentityFromRequest
// (internal/handlers/updates.route.go): the same conversion rules
// (parseLoggedUserSession, reportsNobodyLoggedOn, truncate), just sourced
// from JSON fields instead of X-EMLy-* headers. updater_version/contact
// still come from the upgrade request's User-Agent, and ip from the
// connection - the identity message never carries either.
func clientIdentityFromWSPayload(r *http.Request, p clientWSIdentityPayload) clientIdentity {
	uaVersion, contact := parseUpdaterUserAgent(r.UserAgent())
	state, disconnectedAt := parseLoggedUserSession(p.LoggedUserState, p.LoggedUserDisconnectedAt)
	loggedUser := truncate(p.LoggedUser, 255)
	return clientIdentity{
		HWID:                     p.HWID,
		Hostname:                 p.Hostname,
		ADDomain:                 p.ADDomain,
		LoggedUser:               loggedUser,
		LoggedUserState:          state,
		LoggedUserDisconnectedAt: disconnectedAt,
		NobodyLoggedOn:           reportsNobodyLoggedOn(loggedUser, uaVersion),
		Serial:                   truncate(p.Serial, 128),
		Product:                  truncate(p.Product, 128),
		OSVersion:                truncate(p.OSVersion, 128),
		EMLyVersion:              truncate(p.EMLyVersion, 20),
		UAVersion:                uaVersion,
		Contact:                  contact,
		IP:                       clientIPFromRequest(r),
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/handlers/... -run TestClientIdentityFromWSPayload -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/handlers/client_ws.route.go internal/handlers/client_ws_test.go
git commit -m "feat: add identity payload conversion for GET /v2/client/ws"
```

---

### Task 5: `ClientWS` connection handler — handshake + heartbeat

**Files:**
- Modify: `internal/handlers/client_ws.route.go` (append to the file created in Task 4)
- Modify: `internal/handlers/client_ws_test.go` (append)

**Interfaces:**
- Consumes: `clientIdentityFromWSPayload`/`clientWSInMessage`/`clientWSIdentityPayload` (Task 4), `upsertUpdaterClient(ctx, db, id) (int64, error)` and `clientIdentity.identified() bool` (`internal/handlers/updates.route.go`), `wsEnvelopeOut` and `wsWriteTimeout` (`internal/handlers/stats_stream.route.go`, same package), `presencehub.New/Connect/Disconnect` (Task 1).
- Produces: `ClientWS(db *sqlx.DB, presence *presencehub.Hub) http.HandlerFunc` — the exported handler `internal/routes/v2` mounts in Task 8.

- [ ] **Step 1: Write the failing tests**

```go
// internal/handlers/client_ws_test.go — append
import (
	"context"
	"encoding/json"
	"net/http/httptest" // already imported above
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"emly-api-go/internal/presencehub"
)

// TestClientWSSendsHelloOnConnect checks the first half of the handshake
// (design doc §3.1): the server speaks first. It never sends an identity
// back, so the handler never reaches upsertUpdaterClient - nil db is safe.
func TestClientWSSendsHelloOnConnect(t *testing.T) {
	srv := httptest.NewServer(ClientWS(nil, presencehub.New(presencehub.DefaultGraceDuration)))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var env wsEnvelopeOut
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	if env.Type != "hello" {
		t.Fatalf("first message type = %q, want %q", env.Type, "hello")
	}
}

// TestClientWSRejectsNonIdentityFirstMessage checks that sending anything
// other than "identity" as the first client message is rejected immediately
// (design doc §3.1) rather than left to time out - deterministic and fast,
// and never reaches upsertUpdaterClient, so nil db is safe here too.
func TestClientWSRejectsNonIdentityFirstMessage(t *testing.T) {
	srv := httptest.NewServer(ClientWS(nil, presencehub.New(presencehub.DefaultGraceDuration)))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	if _, _, err := c.Read(ctx); err != nil { // consume "hello"
		t.Fatalf("Read hello: %v", err)
	}

	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("Read error response: %v", err)
	}
	var env wsEnvelopeOut
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	if env.Type != "error" {
		t.Fatalf("response type = %q, want %q", env.Type, "error")
	}

	// The server must close the connection right after: a further read
	// either errors or reports a close.
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("expected the connection to be closed after a non-identity first message")
	}
}

// TestClientWSRejectsUnidentifiedIdentity checks that an identity message
// carrying neither hwid nor hostname is rejected the same way (design doc
// §3.1's "non identificato"). Also never reaches upsertUpdaterClient.
func TestClientWSRejectsUnidentifiedIdentity(t *testing.T) {
	srv := httptest.NewServer(ClientWS(nil, presencehub.New(presencehub.DefaultGraceDuration)))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	if _, _, err := c.Read(ctx); err != nil { // consume "hello"
		t.Fatalf("Read hello: %v", err)
	}

	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"identity","data":{}}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("Read error response: %v", err)
	}
	var env wsEnvelopeOut
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	if env.Type != "error" {
		t.Fatalf("response type = %q, want %q", env.Type, "error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/handlers/... -run TestClientWS -v`
Expected: FAIL (build error — `ClientWS` doesn't exist)

- [ ] **Step 3: Write the implementation**

```go
// internal/handlers/client_ws.route.go — append
import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/presencehub"
)

const (
	// clientWSHelloTimeout bounds how long the server waits for the
	// client's first message (design doc §3.1: "entro 10s").
	clientWSHelloTimeout = 10 * time.Second
	// clientWSPingInterval/clientWSIdleTimeout implement the heartbeat
	// (design doc §3.2): a ping every 10s, and the connection is torn down
	// if nothing at all arrives within 20s (two intervals).
	clientWSPingInterval = 10 * time.Second
	clientWSIdleTimeout  = 20 * time.Second
)

func writeClientWSEnvelope(ctx context.Context, c *websocket.Conn, typ string, data interface{}) error {
	env := wsEnvelopeOut{Type: typ, Data: data}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}

func writeClientWSError(ctx context.Context, c *websocket.Conn, code, message string) {
	_ = writeClientWSEnvelope(ctx, c, "error", map[string]string{"code": code, "message": message})
}

// readClientIdentity reads exactly one message and requires it to be a
// well-formed, identified "identity" message (design doc §3.1). Any other
// outcome sends an `error` envelope and returns ok=false; the caller closes
// the connection.
func readClientIdentity(ctx context.Context, c *websocket.Conn, r *http.Request) (clientIdentity, bool) {
	rctx, cancel := context.WithTimeout(ctx, clientWSHelloTimeout)
	defer cancel()

	_, data, err := c.Read(rctx)
	if err != nil {
		return clientIdentity{}, false
	}

	var msg clientWSInMessage
	if err := json.Unmarshal(data, &msg); err != nil || msg.Type != "identity" {
		writeClientWSError(ctx, c, "unidentified", "expected an identity message")
		return clientIdentity{}, false
	}

	var payload clientWSIdentityPayload
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		writeClientWSError(ctx, c, "invalid_params", "invalid identity payload")
		return clientIdentity{}, false
	}

	id := clientIdentityFromWSPayload(r, payload)
	if !id.identified() {
		writeClientWSError(ctx, c, "unidentified", "identity carries neither hwid nor hostname")
		return clientIdentity{}, false
	}
	return id, true
}

// clientWSReadLoop is the connection's single reader. Each Read is bounded
// by clientWSIdleTimeout, so the loop exits the moment 20s pass without a
// single frame from the client, "pong" included (design doc §3.2). An
// unrecognized message type is logged and ignored, never a reason to close
// (design doc §3.3): that is what lets a future message type roll out
// without breaking already-deployed updaters.
func clientWSReadLoop(ctx context.Context, c *websocket.Conn) {
	for {
		rctx, cancel := context.WithTimeout(ctx, clientWSIdleTimeout)
		_, data, err := c.Read(rctx)
		cancel()
		if err != nil {
			return
		}

		var msg clientWSInMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "pong":
			// Keepalive only - reading anything at all already reset the
			// idle timer above.
		default:
			slog.DebugContext(ctx, "client ws: unrecognized message type", "type", msg.Type)
		}
	}
}

// clientWSPingLoop sends the heartbeat (design doc §3.2). It is the only
// writer once the handshake completes, so no write mutex is needed. It
// exits on ctx cancellation or the first failed write.
func clientWSPingLoop(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(clientWSPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := writeClientWSEnvelope(ctx, c, "ping", nil); err != nil {
				return
			}
		}
	}
}

// ClientWS handles GET /v2/client/ws (design doc §2/§3). Auth
// (apimw.APIKeyAuth) runs before this handler as route middleware, same as
// the updater's self-update manifest - unlike /v2/stats/stream, there is no
// query-string key fallback to justify an inline check here. presence may be
// nil (tests); presencehub.Hub's own methods tolerate that, but Connect on a
// nil Hub panics, so callers that pass nil must not expect presence
// tracking to do anything (only exercised by tests that never send a valid
// identity).
func ClientWS(db *sqlx.DB, presence *presencehub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			slog.WarnContext(r.Context(), "client ws: upgrade failed", "error", err)
			return
		}

		ctx, cancel := context.WithCancel(r.Context())

		if err := writeClientWSEnvelope(ctx, c, "hello", nil); err != nil {
			cancel()
			c.Close(websocket.StatusInternalError, "")
			return
		}

		identity, ok := readClientIdentity(ctx, c, r)
		if !ok {
			cancel()
			c.Close(websocket.StatusPolicyViolation, "identity required")
			return
		}

		clientID, err := upsertUpdaterClient(ctx, db, identity)
		if err != nil {
			slog.WarnContext(ctx, "client ws: failed to upsert client", "error", err)
			cancel()
			c.Close(websocket.StatusInternalError, "")
			return
		}

		tok, supersede := presence.Connect(clientID)
		slog.InfoContext(ctx, "client ws: connection established", "client_id", clientID)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); clientWSPingLoop(ctx, c) }()
		go func() {
			defer wg.Done()
			select {
			case <-supersede:
				cancel()
			case <-ctx.Done():
			}
		}()

		clientWSReadLoop(ctx, c)

		cancel()
		wg.Wait()
		presence.Disconnect(tok)
		c.Close(websocket.StatusNormalClosure, "")
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/handlers/... -run TestClientWS -v`
Expected: PASS

- [ ] **Step 5: Run the full package test suite to check nothing else broke**

Run: `go test ./internal/handlers/... -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/handlers/client_ws.route.go internal/handlers/client_ws_test.go
git commit -m "feat: add GET /v2/client/ws handshake and heartbeat handler"
```

**Manual verification (not covered by unit tests, same convention as `upsertUpdaterClient` elsewhere in this repo — no test in this codebase exercises that function against a live DB):** against a real MySQL, dial `/v2/client/ws` with a valid `X-Api-Key`, send `{"type":"identity","data":{"hwid":"..."}}` after `hello`, confirm a row appears/updates in `updater_clients`, confirm `ping`/`pong` continues every 10s, and confirm dialing again with the same `hwid` closes the first connection (`supersede`).

---

### Task 6: `GET /v2/stats/clients` — decorate `online`

**Files:**
- Modify: `internal/handlers/stats.route.go:214-250` (`ListStatsClients`)
- Test: `internal/handlers/stats_online_test.go`

**Interfaces:**
- Consumes: `presencehub.Hub.Online(int64) bool` (Task 1), `models.UpdaterClient.Online` (Task 3).
- Produces: `decorateOnline(clients []models.UpdaterClient, presence *presencehub.Hub)`; `ListStatsClients(db *sqlx.DB, presence *presencehub.Hub) http.HandlerFunc` (signature change — was `ListStatsClients(db *sqlx.DB)`).

- [ ] **Step 1: Write the failing test**

```go
// internal/handlers/stats_online_test.go
package handlers

import (
	"testing"

	"emly-api-go/internal/models"
	"emly-api-go/internal/presencehub"
)

func TestDecorateOnline(t *testing.T) {
	presence := presencehub.New(presencehub.DefaultGraceDuration)
	tok, _ := presence.Connect(42)
	defer presence.Disconnect(tok)

	clients := []models.UpdaterClient{{ID: 42}, {ID: 7}}
	decorateOnline(clients, presence)

	if !clients[0].Online {
		t.Fatal("client 42 should be Online=true (connected)")
	}
	if clients[1].Online {
		t.Fatal("client 7 should be Online=false (never connected)")
	}
}

func TestDecorateOnlineNilHub(t *testing.T) {
	clients := []models.UpdaterClient{{ID: 42}}
	decorateOnline(clients, nil)
	if clients[0].Online {
		t.Fatal("a nil presence hub must decorate every client as offline")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/handlers/... -run TestDecorateOnline -v`
Expected: FAIL (build error — `decorateOnline` doesn't exist)

- [ ] **Step 3: Add `decorateOnline` and wire it into `ListStatsClients`**

```go
// internal/handlers/stats.route.go — add near fetchAllStatsClients (after line 212)
// decorateOnline sets Online on each client from presence (internal/
// presencehub), the in-memory GET /v2/client/ws connection registry (design
// doc §5). It is a response-time decoration, not a DB column - callers that
// don't need it (GET /v2/stats/clients/{id}) simply don't call this, and
// presence being nil (tests, or a build that never constructs one) decorates
// every client as offline rather than panicking.
func decorateOnline(clients []models.UpdaterClient, presence *presencehub.Hub) {
	for i := range clients {
		clients[i].Online = presence.Online(int64(clients[i].ID))
	}
}
```

```go
// internal/handlers/stats.route.go:214-215 — change the signature:
// ListStatsClients returns a paginated list of known EMLy Updater clients,
// each decorated with its current online state from presence
// (internal/presencehub).
func ListStatsClients(db *sqlx.DB, presence *presencehub.Hub) http.HandlerFunc {
```

```go
// internal/handlers/stats.route.go — inside ListStatsClients, right after the
// existing fetchStatsClientsPage call (originally lines 237-241):
		clients, total, err := fetchStatsClientsPage(r.Context(), db, page, pageSize, onlyOnline, windowMinutes)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch clients")
			return
		}
		decorateOnline(clients, presence)
```

Add the import:

```go
// internal/handlers/stats.route.go — imports
	"emly-api-go/internal/presencehub"
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/handlers/... -run TestDecorateOnline -v`
Expected: PASS

Note: this step leaves `internal/handlers` and `internal/routes/v2` non-building until Task 7/8 update every `ListStatsClients(db)` call site — that's expected mid-plan; Task 8 fixes routing.

- [ ] **Step 5: Commit**

```bash
git add internal/handlers/stats.route.go internal/handlers/stats_online_test.go
git commit -m "feat: decorate GET /v2/stats/clients with online presence"
```

---

### Task 7: `stats:clients` WS channel — decorate `online`

**Files:**
- Modify: `internal/handlers/stats_stream.route.go:87-109` (`wsConn`/`newWSConn`)
- Modify: `internal/handlers/stats_stream.route.go:151-179` (`sendSnapshot`)
- Modify: `internal/handlers/stats_stream.route.go:320-344` (`handleUpdaterEventPush`)
- Modify: `internal/handlers/stats_stream.route.go:439-474` (`StatsStream`)
- Modify: `internal/handlers/stats_stream_test.go` (every `StatsStream(...)` call site)

**Interfaces:**
- Consumes: `decorateOnline` (Task 6), `presencehub.Hub.Online` (Task 1).
- Produces: `StatsStream(db *sqlx.DB, hub *statshub.Hub, presence *presencehub.Hub) http.HandlerFunc` (signature change — was `StatsStream(db *sqlx.DB, hub *statshub.Hub)`); `newWSConn(ws *websocket.Conn, presence *presencehub.Hub) *wsConn` (signature change).

- [ ] **Step 1: Write the failing test**

```go
// internal/handlers/stats_stream_test.go — add, alongside the existing tests
func TestStatsStreamClientsSnapshotIncludesOnline(t *testing.T) {
	// This test only pins the wiring (StatsStream/newWSConn accept and pass
	// through a presence hub); it does not exercise a DB-backed subscribe
	// flow, consistent with this file's existing nil-DB-safe tests.
	h := StatsStream(nil, statshub.New(), presencehub.New(presencehub.DefaultGraceDuration))
	if h == nil {
		t.Fatal("StatsStream returned a nil handler")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/handlers/... -run TestStatsStreamClientsSnapshotIncludesOnline -v`
Expected: FAIL (build error — `StatsStream` takes 2 args, not 3)

- [ ] **Step 3: Thread `presence` through `wsConn`, `newWSConn`, `sendSnapshot`, `handleUpdaterEventPush`, `StatsStream`**

```go
// internal/handlers/stats_stream.route.go:87-94 — add a presence field:
// wsConn is one accepted /v2/stats/stream connection.
type wsConn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex

	subMu sync.Mutex
	sub   wsSubData

	// presence backs the "online" field this connection's stats:clients
	// snapshots/updates carry (design doc for GET /v2/client/ws §5). May be
	// nil (tests); presencehub.Hub.Online tolerates that.
	presence *presencehub.Hub
}
```

```go
// internal/handlers/stats_stream.route.go:96-109 — accept it in the constructor:
func newWSConn(ws *websocket.Conn, presence *presencehub.Hub) *wsConn {
	now := time.Now().UTC()
	return &wsConn{
		ws:       ws,
		presence: presence,
		sub: wsSubData{
			channels:      make(map[string]bool),
			windowMinutes: defaultConnectedWindowMinutes,
			product:       "emly",
			eventsBucket:  "day",
			eventsFrom:    now.AddDate(0, 0, -30),
			eventsTo:      now,
		},
	}
}
```

```go
// internal/handlers/stats_stream.route.go:162-168 — decorate the clients
// snapshot (inside sendSnapshot's channelClients case):
	case channelClients:
		clients, err := fetchAllStatsClients(ctx, db)
		if err != nil {
			cn.sendError(ctx, "internal", "failed to fetch clients")
			return
		}
		decorateOnline(clients, cn.presence)
		_ = cn.send(ctx, "snapshot", channelClients, map[string]interface{}{"clients": clients})
```

```go
// internal/handlers/stats_stream.route.go:332-337 — decorate the pushed
// client (inside handleUpdaterEventPush's channelClients case):
	if s.channels[channelClients] && ev.Client != nil {
		client := *ev.Client
		client.Online = cn.presence.Online(int64(client.ID))
		_ = cn.send(ctx, "update", channelClients, map[string]interface{}{
			"upserted":    []models.UpdaterClient{client},
			"removed_ids": []int{},
		})
	}
```

```go
// internal/handlers/stats_stream.route.go:439-459 — accept and forward presence:
func StatsStream(db *sqlx.DB, hub *statshub.Hub, presence *presencehub.Hub) http.HandlerFunc {
	cfg := config.Load()

	return func(w http.ResponseWriter, r *http.Request) {
		adminKey := r.Header.Get("X-Admin-Key")
		if adminKey == "" {
			adminKey = r.URL.Query().Get("admin_key")
		}
		if cfg.AdminKey == "" || adminKey != cfg.AdminKey {
			jsonError(w, http.StatusUnauthorized, "unauthorized admin key")
			slog.WarnContext(r.Context(), "stats stream: admin key auth failed", "url", r.URL.Path)
			return
		}

		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			slog.WarnContext(r.Context(), "stats stream: upgrade failed", "error", err)
			return
		}

		cn := newWSConn(c, presence)
```

Add the import:

```go
// internal/handlers/stats_stream.route.go — imports
	"emly-api-go/internal/presencehub"
```

- [ ] **Step 4: Fix the existing `StatsStream(...)` call sites in `stats_stream_test.go`**

Every existing call (`TestStatsStreamRejectsMissingOrWrongAdminKey`, `TestStatsStreamAcceptsQueryStringKeyFallback`, `TestStatsStreamPingPong`, `TestStatsStreamDialRejectedWithoutAdminKey`) currently reads `StatsStream(nil, statshub.New())`. Update each to:

```go
StatsStream(nil, statshub.New(), presencehub.New(presencehub.DefaultGraceDuration))
```

and add the import:

```go
// internal/handlers/stats_stream_test.go — imports
	"emly-api-go/internal/presencehub"
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/handlers/... -v`
Expected: PASS (this package now builds and passes end-to-end; Task 8 still needs to fix `internal/routes/v2`'s callers)

- [ ] **Step 6: Commit**

```bash
git add internal/handlers/stats_stream.route.go internal/handlers/stats_stream_test.go
git commit -m "feat: decorate stats:clients WS channel with online presence"
```

---

### Task 8: Route wiring — `internal/routes/v2/client.go` + signature updates

**Files:**
- Create: `internal/routes/v2/client.go`
- Create: `internal/routes/v2/client_routing_test.go`
- Modify: `internal/routes/v2/stats.go:16-45` (`registerStats`)
- Modify: `internal/routes/v2/v2.go:16-54` (`NewRouter`)
- Modify: `internal/routes/v2/stats_stream_routing_test.go`, `internal/routes/v2/config_routing_test.go`, `internal/routes/v2/updater_routing_test.go` (existing `NewRouter(...)` call sites)

**Interfaces:**
- Consumes: `handlers.ClientWS` (Task 5), `handlers.ListStatsClients`/`handlers.StatsStream` (Tasks 6-7), `apimw.APIKeyAuth`/`apimw.RouteLimitByIP` (`internal/middleware`), `presencehub.Hub` (Task 1).
- Produces: `registerClient(r chi.Router, db *sqlx.DB, presence *presencehub.Hub)`; `NewRouter(db *sqlx.DB, apiFileS3conn, updatesS3conn *storage.S3Connector, configMirror handlers.ConfigMirrorReporter, hub *statshub.Hub, presence *presencehub.Hub, bans handlers.BanReloader) http.Handler` (signature change — `presence` is the new 6th parameter, before `bans`).

- [ ] **Step 1: Write the failing routing test**

```go
// internal/routes/v2/client_routing_test.go
package v2

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClientWSRouteRequiresAPIKey checks that GET /v2/client/ws is mounted
// (not 404) and gates on the API key before attempting any upgrade, via
// apimw.APIKeyAuth like the updater's self-update manifest - unlike
// /v2/stats/stream, there is no query-string key fallback here.
func TestClientWSRouteRequiresAPIKey(t *testing.T) {
	router := NewRouter(nil, nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/client/ws", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatalf("GET /client/ws returned 404; route not mounted")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /client/ws without a key = %d, want %d (body: %s)", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/routes/v2/... -run TestClientWSRouteRequiresAPIKey -v`
Expected: FAIL (build error — `NewRouter` still takes 6 args, and the route doesn't exist)

- [ ] **Step 3: Create `registerClient` and wire it into `NewRouter`**

```go
// internal/routes/v2/client.go
package v2

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/handlers"
	"emly-api-go/internal/presencehub"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// registerClient mounts GET /v2/client/ws, the EMLy Updater's persistent
// presence channel
// (docs/superpowers/specs/2026-09-17-client-presence-ws-api-design.md).
// presence may be nil (tests, or a build that never constructs one);
// handlers.ClientWS and presencehub.Hub's own methods tolerate that.
func registerClient(r chi.Router, db *sqlx.DB, presence *presencehub.Hub) {
	r.Route("/client", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/ws", handlers.ClientWS(db, presence))
		})
	})
}
```

```go
// internal/routes/v2/stats.go:16-26 — registerStats gains a presence param:
// registerStats mounts both the REST stats/* endpoints (admin-key gated,
// like the rest of this group) and their real-time counterpart,
// /stats/stream. hub may be nil (tests, or a build with the WS stream
// unused); handlers.StatsStream and recordUpdaterEvent both tolerate that.
// presence backs the "online" field on GET /stats/clients and the
// stats:clients WS channel; nil is fine (see internal/presencehub). cfg
// carries StatsCacheTTL, which bounds how stale the polled /summary may be -
// see handlers.GetStatsSummary.
func registerStats(r chi.Router, db *sqlx.DB, cfg *config.Config, hub *statshub.Hub, presence *presencehub.Hub) {
	r.Route("/stats", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/summary", handlers.GetStatsSummary(db, cfg))
			r.Get("/clients", handlers.ListStatsClients(db, presence))
			r.Get("/clients/{id}", handlers.GetStatsClientDetail(db))
			r.Get("/events", handlers.GetStatsEvents(db))
		})

		r.Group(func(r chi.Router) {
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/stream", handlers.StatsStream(db, hub, presence))
		})
	})
}
```

Add the import `"emly-api-go/internal/presencehub"` to `internal/routes/v2/stats.go`.

```go
// internal/routes/v2/v2.go:16-54 — NewRouter gains a presence param:
// NewRouter returns a chi.Router with all /v2 routes mounted. apiFileS3conn
// backs bug-report file attachments and updatesS3conn backs update-release
// installers; the two are independent connectors and may live on different
// S3-compatible providers. configMirror is non-nil only on a site mirror
// (CONFIG_UPSTREAM_URL set) and adds its replication state to /v2/health;
// pass nil on the cloud/primary instance and in tests. hub feeds
// /v2/stats/stream (nil is fine - the route degrades to snapshots-only, no
// pushed updates; see statshub and handlers.StatsStream). presence backs
// GET /v2/client/ws and the "online" field on GET /v2/stats/clients /
// stats:clients (nil is fine; see internal/presencehub). bans is the live
// block-list snapshot the admin routes refresh after a write; nil is fine in
// tests, where the write lands in the table and nothing needs to enforce it.
func NewRouter(db *sqlx.DB, apiFileS3conn, updatesS3conn *storage.S3Connector, configMirror handlers.ConfigMirrorReporter, hub *statshub.Hub, presence *presencehub.Hub, bans handlers.BanReloader) http.Handler {
	r := chi.NewRouter()

	rl := emlyMiddleware.NewRateLimiter(config.Load())

	r.Use(rl.Handler)

	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Server", "emly-api-go")
			w.Header().Set("X-Powered-By", "Rexouium in a suit")
			next.ServeHTTP(w, r)
		})
	})

	r.Get("/health", handlers.HealthWithConfigMirror(db, configMirror))

	registerUpdates(r, db, updatesS3conn, config.Load().UpdatesS3Prefix, config.Load().UpdaterS3Prefix, hub)
	registerStats(r, db, config.Load(), hub, presence)
	registerConfig(r, db, config.Load())
	registerBans(r, db, bans)
	registerClient(r, db, presence)

	r.Route("/api", func(r chi.Router) {
		registerAdmin(r, db)
		registerBugReports(r, db, config.Load().Database, apiFileS3conn)
	})

	return r
}
```

Add the import `"emly-api-go/internal/presencehub"` to `internal/routes/v2/v2.go`.

- [ ] **Step 4: Fix every other `NewRouter(...)` call site to pass a 6th `nil`**

`internal/routes/v2/stats_stream_routing_test.go:16`, `internal/routes/v2/config_routing_test.go:15,54,78`, `internal/routes/v2/updater_routing_test.go:29,103` all currently read:

```go
router := NewRouter(nil, nil, nil, nil, nil, nil)
```

Change each to:

```go
router := NewRouter(nil, nil, nil, nil, nil, nil, nil)
```

(6 occurrences total across the 3 files.)

- [ ] **Step 5: Run the full `v2` routing test suite**

Run: `go test ./internal/routes/... -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/routes/v2/client.go internal/routes/v2/client_routing_test.go \
        internal/routes/v2/stats.go internal/routes/v2/v2.go \
        internal/routes/v2/stats_stream_routing_test.go \
        internal/routes/v2/config_routing_test.go \
        internal/routes/v2/updater_routing_test.go
git commit -m "feat: mount GET /v2/client/ws and thread presence into stats routes"
```

---

### Task 9: `main.go` wiring — construct the hub, mux bypass

**Files:**
- Modify: `internal/routes/routes.go` (`RegisterAll`)
- Modify: `main.go:222-224` (statshub construction)
- Modify: `main.go:251` (`routes.RegisterAll` call)
- Modify: `main.go:270-276` (the hijack-safe `mux` bypass)

**Interfaces:**
- Consumes: `presencehub.New`/`DefaultGraceDuration` (Task 1), `v2.NewRouter`'s new signature (Task 8).
- Produces: `routes.RegisterAll(r chi.Router, db *sqlx.DB, apiFileS3conn, updatesS3conn *storage.S3Connector, configMirror handlers.ConfigMirrorReporter, statsHub *statshub.Hub, presence *presencehub.Hub, bans handlers.BanReloader)` (signature change — `presence` is the new 6th parameter, before `bans`); the running binary now serves `GET /v2/client/ws` through the same hijack-safe path as `/v2/stats/stream`.

- [ ] **Step 1: Update `RegisterAll`**

```go
// internal/routes/routes.go — replace the whole file's relevant parts:
import (
	"emly-api-go/internal/config"
	"emly-api-go/internal/handlers"
	apimw "emly-api-go/internal/middleware"
	"emly-api-go/internal/presencehub"
	"emly-api-go/internal/statshub"
	"net/http"
	"time"

	v1 "emly-api-go/internal/routes/v1"
	v2 "emly-api-go/internal/routes/v2"
	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// RegisterAll mounts every versioned API onto the root router. apiFileS3conn
// and updatesS3conn are independent connectors — each may be nil, and each
// may point at a bucket on a different S3-compatible host/service/provider.
// configMirror is non-nil only on a site mirror (CONFIG_UPSTREAM_URL set);
// pass nil on the cloud/primary instance. statsHub feeds /v2/stats/stream;
// pass nil to run without the real-time stats stream (it degrades to
// snapshots-only, see statshub). presence backs GET /v2/client/ws and the
// "online" field on the stats routes; pass nil to run without presence
// tracking (see internal/presencehub). bans is the live permanent-block
// snapshot the /v2/bans admin routes refresh after a write.
func RegisterAll(r chi.Router, db *sqlx.DB, apiFileS3conn, updatesS3conn *storage.S3Connector, configMirror handlers.ConfigMirrorReporter, statsHub *statshub.Hub, presence *presencehub.Hub, bans handlers.BanReloader) {
	dbName := config.Load().Database

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("emly-api-go"))
		if err != nil {
			return
		}
	})

	r.Mount("/v1", v1.NewRouter(db, apiFileS3conn))
	r.Mount("/v2", v2.NewRouter(db, apiFileS3conn, updatesS3conn, configMirror, statsHub, presence, bans))
	// Redirect /health to /v1/health
	r.Get("/health", handlers.Health(db))

	// Legacy compatibility: expose bug-report creation also under /api/bug-reports.
	r.Post("/api/bug-reports", registerBugReports(r, db, dbName, apiFileS3conn))

}
```

(`registerBugReports` below is unchanged.)

- [ ] **Step 2: Construct the hub and update the `RegisterAll` call in `main.go`**

```go
// main.go:220-224 — right after statsHub is constructed:
	statsHub := statshub.New()
	go statsHub.Run(backgroundCtx, cfg.StatsStreamTickInterval)

	// In-memory registry of which EMLy Updater clients currently hold an
	// open GET /v2/client/ws connection (see internal/presencehub package
	// doc). Single-instance, like statsHub above.
	presenceHub := presencehub.New(presencehub.DefaultGraceDuration)
```

```go
// main.go:251 — update the call:
	routes.RegisterAll(r, db, apiFileS3conn, updatesS3conn, configMirrorState, statsHub, presenceHub, bans)
```

Add the import `"emly-api-go/internal/presencehub"` to `main.go`.

- [ ] **Step 3: Add the `/v2/client/ws` bypass next to `/v2/stats/stream`**

```go
// main.go:270-277 — extend the mux section:
	wsHandler := emlyMiddleware.RouteLimitByIP(30, time.Minute)(handlers.StatsStream(db, statsHub, presenceHub))
	wsHandler = rl.Handler(wsHandler)
	wsHandler = chiMiddleware.Recoverer(wsHandler)
	wsHandler = chiMiddleware.RealIP(wsHandler)
	wsHandler = chiMiddleware.RequestID(wsHandler)

	// GET /v2/client/ws hijacks the connection the same way and for the same
	// reason (see the comment above): apimw.APIKeyAuth replaces the inline
	// admin-key check /v2/stats/stream needs, since this route has no
	// query-string key fallback to support.
	clientWSHandler := emlyMiddleware.RouteLimitByIP(30, time.Minute)(handlers.ClientWS(db, presenceHub))
	clientWSHandler = emlyMiddleware.APIKeyAuth(db)(clientWSHandler)
	clientWSHandler = rl.Handler(clientWSHandler)
	clientWSHandler = chiMiddleware.Recoverer(clientWSHandler)
	clientWSHandler = chiMiddleware.RealIP(clientWSHandler)
	clientWSHandler = chiMiddleware.RequestID(clientWSHandler)

	mux := http.NewServeMux()
	mux.Handle("/v2/stats/stream", wsHandler)
	mux.Handle("/v2/client/ws", clientWSHandler)
	mux.Handle("/", r)
```

- [ ] **Step 4: Build and run the full suite**

Run: `go build ./... && go test ./... -v`
Expected: PASS across the whole module.

- [ ] **Step 5: Commit**

```bash
git add internal/routes/routes.go main.go
git commit -m "feat: wire presencehub end-to-end and mount /v2/client/ws in the hijack-safe mux"
```

---

### Task 10: Documentation — `CLAUDE.md` + `ROUTES.md`

**Files:**
- Modify: `CLAUDE.md`
- Modify: `ROUTES.md`

**Interfaces:** none (docs only).

- [ ] **Step 1: Update `CLAUDE.md`**

In the `### Package layout` section, add a bullet after the `internal/statshub/` entry:

```markdown
- `internal/presencehub/` — HTTP- and DB-free like `internal/statshub`: an in-process `Hub` tracking which EMLy Updater clients currently hold an open `GET /v2/client/ws` connection, with a short grace period on disconnect so a brief drop/reconnect never flickers "offline". Single-instance only, same limit as `statshub`. `internal/handlers` (`ClientWS`, and the `online` decoration on `GET /v2/stats/clients` / the `stats:clients` WS channel) is its only caller; `main.go` constructs the one `Hub`.
```

In the `### Routing — versioned` section's v2 bullet list, add after the `stats/` sentence:

```markdown
v2 also carries `client/` (`client.go`, `internal/handlers/client_ws.route.go`): an API-key-protected `GET /v2/client/ws` the EMLy Updater holds open for the lifetime of the service — a `hello`/`identity` handshake reusing the same `clientIdentity`/`upsertUpdaterClient` machinery as manifest/download, then a 10s server-ping heartbeat. Presence is tracked in-memory (`internal/presencehub`) and surfaced as `online` on `GET /v2/stats/clients` and the `stats:clients` WS channel. See `docs/superpowers/specs/2026-09-17-client-presence-ws-api-design.md`. Gated client-side by the `clientWs.enabled` kill switch in the remote-config document; the API does not itself refuse a connection when that flag is false.
```

In the `### Handler conventions` section, add a bullet near the installer-download/manifest conventions:

```markdown
- `GET /v2/client/ws`'s envelope `type` field is deliberately open-ended: today only `hello`/`identity`/`ping`/`pong` exist, and an unrecognized `type` on either side is logged and ignored rather than closing the connection — that is what lets a future message type (e.g. a server→client command) roll out without breaking an already-deployed updater.
```

- [ ] **Step 2: Update `ROUTES.md`**

Add to the index (after the "Stream WebSocket" line):

```markdown
   - [Presenza client](#59-presenza-client--v2clientws)
```

Add a new `### 5.9` section after `### 5.8 Stream WebSocket` (before `## 6. Riepilogo autenticazione`):

```markdown
### 5.9 Presenza client — `/v2/client/ws`

| Metodo | Path              | Auth      | Cosa fa |
|--------|-------------------|-----------|---------|
| `GET`  | `/v2/client/ws`   | `API (WS)` | Upgrade WebSocket che l'EMLy Updater tiene aperta per tutta la vita del servizio, per la presenza online/offline in tempo reale. |

L'autenticazione (`X-Api-Key`) avviene tramite lo stesso middleware del
manifest self-update dell'Updater, prima dell'upgrade — a differenza di
`/v2/stats/stream` non esiste un fallback in query string.

**Handshake**

```
server ──► { "type": "hello" }
client ──► { "type": "identity", "data": { hwid, hostname, ad_domain,
             logged_user, logged_user_state, logged_user_disconnected_at,
             serial, product, os_version, emly_version } }
```

L'identità arriva nel payload invece che negli header `X-EMLy-*`, ma passa
per lo stesso upsert di manifest/download: la riga in `updater_clients` è
la stessa. Un'identità mancante o senza `hwid`/`hostname` entro 10s riceve
un `error` e la connessione viene chiusa.

**Heartbeat**: il server manda `{"type":"ping"}` ogni 10s; il client deve
rispondere `{"type":"pong"}` entro 20s o la connessione è considerata morta.
Un `type` sconosciuto da uno dei due lati viene ignorato, non chiude la
connessione — è così che un futuro comando si aggiunge senza rompere un
Updater già distribuito.

**Presenza**: tracciata solo in memoria (`internal/presencehub`), con una
finestra di grazia di 15s alla disconnessione prima di segnare il client
offline — assorbe un calo di rete breve o una riconnessione. Una nuova
connessione con lo stesso client sostituisce quella precedente, che viene
chiusa dal server.

Il campo `"online"` che questo canale alimenta compare in
`GET /v2/stats/clients` e nel canale `stats:clients` di
`GET /v2/stats/stream` (§5.7-5.8): è calcolato al momento della risposta dal
presence hub, **non** dallo stesso filtro di `last_seen_at` che
`?online=true` su `/v2/stats/clients` usa per decidere quali righe
restituire — i due possono disaccordare per un client appena disconnesso
ma ancora dentro la finestra di `last_seen_at`.

Attivata lato client dal flag `clientWs.enabled` nel documento di
configurazione remota (§5.5): l'API non rifiuta comunque una connessione se
quel flag è `false`, è l'Updater a non aprirla.
```

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md ROUTES.md
git commit -m "docs: document GET /v2/client/ws and the online presence field"
```

---

## Plan Self-Review Notes

- **Spec coverage:** §2 (endpoint) → Task 8; §3 (protocol/handshake/heartbeat) → Tasks 4-5; §4 (identity/upsert reuse) → Task 4-5; §5 (presence hub, grace period, supersede, `online` decoration) → Tasks 1, 6, 7; §6 (manifest poll unchanged) → no code change needed, nothing to do; §7 (mux/hijack bypass) → Task 9; §8 (kill switch, no env vars) → Task 2; §9 (packages) → Tasks 1, 4/5, 8; §11 (testing) → each task's own test step, with the DB-touching gap called out explicitly in Task 5 rather than glossed over.
- **Placeholder scan:** none — every step has runnable code, every test has real assertions.
- **Type consistency:** `clientIdentity`, `clientWSIdentityPayload`, `clientWSInMessage`, `presencehub.Token`, `presencehub.Hub`, `ListStatsClients`, `StatsStream`, `NewRouter`, `RegisterAll` signatures are consistent across every task that references them (cross-checked against Tasks 1, 4, 5, 6, 7, 8, 9).
