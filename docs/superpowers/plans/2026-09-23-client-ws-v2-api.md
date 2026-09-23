# Client WS protocol v2 — API side Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend `GET /v2/client/ws` from the v1 presence protocol to the v2 protocol of `CLIENT_WS_PROTOCOL.md` — capability negotiation, server→client commands with ack/result, client→server events, server→client notify — plus the admin REST surface that issues commands and reads their outcome.

**Architecture:** Three new layers, each testable on its own. `internal/clientproto` is the wire format (types, names, IDs, argument validation), pure Go and copied by shape into the updater. `internal/clienthub` is the in-memory registry of v2 sessions, commands and recent events, HTTP- and DB-free like `internal/presencehub`. `internal/clientws` keeps the handshake/heartbeat and becomes the dispatcher between the socket and the hub, and gains the admin routes. Nothing is persisted in this plan: command records and events live in memory for 24h, the same single-instance limit `presencehub` already documents.

**Tech Stack:** Go 1.26, `github.com/coder/websocket` v1.8.15 (its `Conn` methods are safe for concurrent use except `Read`/`Reader`, which is what lets the ping loop and a command send share one socket without a writer goroutine), chi v5, sqlx/MySQL.

**Spec:** `CLIENT_WS_PROTOCOL.md` (repo root, branch `docs/client-ws-protocol`). Twin plan for the other side: `emly-updater/docs/superpowers/plans/2026-09-23-client-ws-v2-updater.md`.

## Global Constraints

- v1 clients must keep working byte-for-byte: a client whose `identity` has no `protocol` (or `protocol < 2`) never receives `welcome`, `command` or `notify`.
- Field names on the wire are `snake_case`; names/topics/codes are lowercase, `.` for namespace, `_` inside a word.
- IDs are ULIDs (26 chars, Crockford base32). `ping`/`pong` carry no `id`.
- `max_message_bytes` = 65536, `max_events_per_minute` = 60, `ack_timeout_seconds` = 5 (`clientproto.DefaultLimits`).
- Unknown `type` → ignored (Debug log). Unknown event `name` / notify `topic` → recorded, not an error. Unknown command `name` is refused *before* sending (422 on the REST call).
- An `ack`/`result` only acts on a command issued to the same `updater_clients.id` as the connection it arrives on (§12.4).
- `session.changed` with `changed: true` updates only `logged_user`, `logged_user_state`, `logged_user_disconnected_at` — never `last_seen_at`.
- Command TTL: default 600s, max 86400s.
- No new dependency. No DB migration.
- `ROUTES.md`, `DOCS.md` (Italian, Node/PHP analogies), `CLAUDE.md` updated in the same branch (repo rule "Documentation upkeep").
- `testdata/remoteconfig/` fixtures touched here must be copied verbatim to `emly-updater/testdata/remoteconfig/` (the updater plan's Task 3 does the same files).
- Commits without any `Co-Authored-By` trailer.

## Review Focus

1. **A v1 updater connecting to the new server** — must see exactly `hello` (now with a `data` it ignores), then pings; never `welcome`. Test: `TestClientWSV1ClientGetsNoWelcome` (Task 6).
2. **An `ack`/`result` whose `reply_to` names another machine's command** — must be ignored, the command stays untouched. Test: `TestHubIgnoresAckFromAnotherClient` (Task 5).
3. **Admin issues a command to a client that disconnected a second ago / reconnected with a new connection** — the command goes to the *current* session; after detach of an old superseded session the new one must stay attached. Test: `TestHubDetachOfSupersededSessionKeepsNewOne` (Task 5).
4. **A flood of events from one connection** — 61st event inside a minute gets `error rate_limited` and close 1008; `pong`/`ack`/`result` never count. Test: `TestClientWSEventRateLimit` (Task 6).
5. **`machine.reboot` never answering with a `result`** — it must end `done` when `service.started` lists its id after reconnection, and `timeout` (not `failed`) if it never does. Tests: `TestHubCompleteByRestart`, `TestHubRebootTimesOutWithoutRestart` (Task 5).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/clientproto/id.go` (new) | `NewID()` ULID generator. Copied verbatim into `emly-updater/internal/wsclient/id.go`. |
| `internal/clientproto/protocol.go` (new) | Envelope, message types, payload structs, limits, names, error codes, command specs. |
| `internal/clientproto/validate.go` (new) | `ValidateArgs(name, raw)` + `ResultTimeout(name, raw)`. |
| `internal/clientproto/*_test.go` (new) | Unit tests. |
| `internal/remoteconfig/types.go`, `parse.go` | `clientWs.commands` allowlist + validation. |
| `testdata/remoteconfig/valid/full.json`, `invalid/clientws-unknown-command*.json` | Shared fixtures. |
| `internal/updaterclient/updaterclient.go` | `LoggedUserFromWS`, `UpdateLoggedUser`. |
| `internal/clienthub/clienthub.go` (new) | Sessions, command lifecycle, event ring, notify fan-out. |
| `internal/clienthub/clienthub_test.go` (new) | Unit tests with a fake clock and fake senders. |
| `internal/clientws/client_ws.route.go` | v2 handshake, dispatch, limits. |
| `internal/clientws/events.go` (new) | Per-event side effects (session.changed, service.started). |
| `internal/clientws/admin.route.go` (new) | `POST /v2/client/{client_id}/commands`, `GET /v2/client/commands/{command_id}`, `GET /v2/client/{client_id}/events`, `POST /v2/client/notify`. |
| `internal/clientws/routes.go` | Mount the admin group. |
| `internal/configapi/routes.go`, `config.route.go` | Notify `config.published` after a publish/rollback. |
| `internal/routes/routes.go`, `internal/routes/v2/v2.go`, `main.go` | Thread `*clienthub.Hub`. |
| `CLIENT_WS_PROTOCOL.md`, `ROUTES.md`, `DOCS.md`, `CLAUDE.md` | Docs. |

---

### Task 0: Branch

- [ ] **Step 1: Create the feature branch from the protocol doc branch**

The working tree may be on `stable`; check it is clean first.

```bash
cd C:/Users/LyzCoote/Desktop/3gIT/EMLy/emly-go-api
git status --short        # must print nothing
git switch docs/client-ws-protocol
git switch -c feat/client-ws-v2
```

- [ ] **Step 2: Commit this plan**

```bash
git add docs/superpowers/plans/2026-09-23-client-ws-v2-api.md
git commit -m "docs: add implementation plan for client ws protocol v2 (API)"
```

---

### Task 1: `clientproto.NewID` (ULID)

**Files:**
- Create: `internal/clientproto/id.go`
- Test: `internal/clientproto/id_test.go`

**Interfaces:**
- Produces: `func NewID() string`, `func newIDAt(t time.Time, entropy io.Reader) string`

- [ ] **Step 1: Write the failing test**

```go
package clientproto

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

type fillReader byte

func (f fillReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(f)
	}
	return len(p), nil
}

// The two ends of the ULID space are fixed by the spec
// (github.com/ulid/spec): all-zero bits, and a 48-bit time plus 80 bits
// of entropy all set, which is the largest valid ULID.
func TestNewIDBounds(t *testing.T) {
	if got := newIDAt(time.UnixMilli(0), fillReader(0)); got != strings.Repeat("0", 26) {
		t.Errorf("zero ULID = %q", got)
	}
	if got := newIDAt(time.UnixMilli(1<<48-1), fillReader(0xFF)); got != "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Errorf("max ULID = %q", got)
	}
}

// The first 10 characters are the millisecond timestamp.
func TestNewIDTimePrefix(t *testing.T) {
	if got := newIDAt(time.UnixMilli(1), fillReader(0))[:10]; got != "0000000001" {
		t.Errorf("time prefix of ms=1 = %q", got)
	}
}

func TestNewIDSortsByTime(t *testing.T) {
	a := newIDAt(time.UnixMilli(1_700_000_000_000), fillReader(0xFF))
	b := newIDAt(time.UnixMilli(1_700_000_000_001), fillReader(0))
	if !(a < b) {
		t.Errorf("%q should sort before %q", a, b)
	}
}

func TestNewIDShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID()
		if len(id) != 26 {
			t.Fatalf("len(%q) = %d", id, len(id))
		}
		for _, c := range []byte(id) {
			if !bytes.ContainsRune([]byte(crockford), rune(c)) {
				t.Fatalf("%q contains %q, not Crockford base32", id, c)
			}
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/clientproto/ -run NewID -v`
Expected: FAIL — `undefined: newIDAt`.

- [ ] **Step 3: Implement**

```go
// Package clientproto is the wire format of GET /v2/client/ws: message
// types, payloads, names and the rules for command arguments
// (CLIENT_WS_PROTOCOL.md). It has no HTTP, no database and no state, so
// the same shapes can be mirrored in emly-updater/internal/wsclient and
// tested on both sides.
package clientproto

import (
	"crypto/rand"
	"io"
	"time"
)

// crockford is the ULID alphabet: base32 without I, L, O, U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns a ULID: 48 bits of Unix milliseconds followed by 80 random
// bits, as 26 Crockford base32 characters. IDs sort by creation time, which
// is what makes a log of commands and events readable without a timestamp.
//
// This file is copied verbatim into emly-updater/internal/wsclient/id.go
// (package name aside): both ends mint IDs and they must look the same.
func NewID() string { return newIDAt(time.Now(), rand.Reader) }

func newIDAt(t time.Time, entropy io.Reader) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	if _, err := io.ReadFull(entropy, b[6:]); err != nil {
		// crypto/rand does not fail on supported platforms; a zero tail
		// still yields a valid, time-ordered ID.
		clear(b[6:])
	}
	// 128 bits encode into 26 characters (130 bits): the stream is read as
	// if it had two leading zero bits.
	var out [26]byte
	for i := range out {
		var v byte
		for j := 0; j < 5; j++ {
			p := i*5 + j - 2
			v <<= 1
			if p >= 0 && b[p/8]&(0x80>>(p%8)) != 0 {
				v |= 1
			}
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/clientproto/ -run NewID -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/clientproto/id.go internal/clientproto/id_test.go
git commit -m "feat(clientproto): add ULID generator for client ws message ids"
```

---

### Task 2: `clientproto` wire types, names and argument validation

**Files:**
- Create: `internal/clientproto/protocol.go`
- Create: `internal/clientproto/validate.go`
- Test: `internal/clientproto/protocol_test.go`

**Interfaces:**
- Consumes: `NewID()` (Task 1).
- Produces (used by Tasks 5–7):
  - consts `ProtocolV1=1`, `ProtocolV2=2`, `ProtocolCurrent=2`; `Type*` message types; `Cmd*`, `Evt*`, `Topic*` names; `Err*` error codes.
  - `type Envelope struct{Type, ID, ReplyTo, TS string; Data json.RawMessage}`
  - `func Frame(typ, replyTo string, data any, now time.Time) ([]byte, error)`
  - `type Hello`, `IdentityExt`, `Limits`, `Welcome`, `ErrorBody`, `Command`, `Ack`, `Result`, `Event`, `Notify`, `UserSession`, `SessionChanged`, `ServiceStarted`, `ReleasePublished`, `ConfigPublished`
  - `var DefaultLimits Limits`, `var ServerCapabilities []string`
  - `type CommandSpec struct{ResultTimeout time.Duration; Destructive, ResultViaEvent bool}`, `var Commands map[string]CommandSpec`
  - `func ValidateArgs(name string, raw json.RawMessage) *ErrorBody`
  - `func ResultTimeout(name string, raw json.RawMessage) time.Duration`
  - `func Intersect(offered []string, supported []string) []string`

- [ ] **Step 1: Write the failing tests**

```go
package clientproto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFrameV1ShapeForPing(t *testing.T) {
	b, err := Frame(TypePing, "", nil, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"type":"ping"}` {
		t.Errorf("ping frame = %s, want the exact v1 bytes", b)
	}
}

func TestFrameCarriesIDTSAndData(t *testing.T) {
	now := time.Date(2026, 9, 23, 8, 15, 2, 123e6, time.UTC)
	b, err := Frame(TypeCommand, "", Command{Name: CmdMachineInfo, ExpiresAt: "2026-09-23T08:25:02Z"}, now)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeCommand || len(env.ID) != 26 || env.TS != "2026-09-23T08:15:02.123Z" {
		t.Errorf("envelope = %+v", env)
	}
	if !strings.Contains(string(env.Data), `"name":"machine.info"`) {
		t.Errorf("data = %s", env.Data)
	}
}

func TestFrameReplyTo(t *testing.T) {
	b, _ := Frame(TypeAck, "01J8ZQ6T3M6X9K2V7B4N1C5D8E", Ack{Accepted: true}, time.Now())
	if !strings.Contains(string(b), `"reply_to":"01J8ZQ6T3M6X9K2V7B4N1C5D8E"`) {
		t.Errorf("frame = %s", b)
	}
}

func TestValidateArgs(t *testing.T) {
	cases := []struct {
		name, args string
		wantCode   string // "" = valid
	}{
		{CmdMachineInfo, ``, ""},
		{CmdMachineInfo, `{}`, ""},
		{CmdMachineInfo, `{"sections":["network","hardware"]}`, ""},
		{CmdMachineInfo, `{"sections":["gpu"]}`, ErrInvalidArgs},
		{CmdMachineInfo, `{"verbose":true}`, ErrInvalidArgs},
		{CmdEMLyManifestCheck, `{}`, ""},
		{CmdEMLyManifestCheck, `{"force":true}`, ErrInvalidArgs},
		{CmdUpdaterManifestCheck, ``, ""},
		{CmdAppsListUpgradable, `{}`, ""},
		{CmdServiceRestart, `{}`, ""},
		{CmdMachineReboot, `{}`, ""},
		{CmdMachineReboot, `{"delay_seconds":0,"when_user_active":"skip"}`, ""},
		{CmdMachineReboot, `{"delay_seconds":3600}`, ""},
		{CmdMachineReboot, `{"delay_seconds":3601}`, ErrInvalidArgs},
		{CmdMachineReboot, `{"delay_seconds":-1}`, ErrInvalidArgs},
		{CmdMachineReboot, `{"when_user_active":"force"}`, ErrInvalidArgs},
		{CmdMachineReboot, `{"message":"hi"}`, ErrInvalidArgs},
		{"machine.format_disk", `{}`, ErrUnsupportedCommand},
		{CmdMachineInfo, `[1]`, ErrInvalidArgs},
	}
	for _, c := range cases {
		got := ValidateArgs(c.name, json.RawMessage(c.args))
		switch {
		case c.wantCode == "" && got != nil:
			t.Errorf("ValidateArgs(%s, %s) = %+v, want valid", c.name, c.args, got)
		case c.wantCode != "" && (got == nil || got.Code != c.wantCode):
			t.Errorf("ValidateArgs(%s, %s) = %+v, want %s", c.name, c.args, got, c.wantCode)
		}
	}
}

func TestResultTimeout(t *testing.T) {
	if got := ResultTimeout(CmdAppsListUpgradable, nil); got != 180*time.Second {
		t.Errorf("apps.list_upgradable = %v", got)
	}
	if got := ResultTimeout(CmdMachineReboot, json.RawMessage(`{"delay_seconds":600}`)); got != 600*time.Second+15*time.Minute {
		t.Errorf("machine.reboot 600 = %v", got)
	}
	if got := ResultTimeout(CmdMachineReboot, nil); got != 300*time.Second+15*time.Minute {
		t.Errorf("machine.reboot default = %v", got)
	}
}

func TestIntersectKeepsOfferedOrderAndDropsUnknown(t *testing.T) {
	got := Intersect([]string{"b", "zzz", "a"}, []string{"a", "b", "c"})
	if strings.Join(got, ",") != "b,a" {
		t.Errorf("Intersect = %v", got)
	}
}

func TestEveryCommandHasASpec(t *testing.T) {
	for _, name := range []string{CmdMachineInfo, CmdEMLyManifestCheck, CmdUpdaterManifestCheck,
		CmdAppsListUpgradable, CmdServiceRestart, CmdMachineReboot} {
		if _, ok := Commands[name]; !ok {
			t.Errorf("no CommandSpec for %s", name)
		}
	}
	if !Commands[CmdMachineReboot].Destructive || !Commands[CmdServiceRestart].ResultViaEvent {
		t.Error("destructive/result-via-event flags wrong")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/clientproto/ -v`
Expected: FAIL — `undefined: Frame`.

- [ ] **Step 3: Implement `protocol.go`**

```go
package clientproto

import (
	"encoding/json"
	"time"
)

// Protocol versions (CLIENT_WS_PROTOCOL.md §4). A client that sends no
// "protocol" in its identity is v1.
const (
	ProtocolV1      = 1
	ProtocolV2      = 2
	ProtocolCurrent = ProtocolV2
)

// Message types (§5).
const (
	TypeHello    = "hello"
	TypeIdentity = "identity"
	TypePing     = "ping"
	TypePong     = "pong"
	TypeError    = "error"
	TypeWelcome  = "welcome"
	TypeCommand  = "command"
	TypeAck      = "ack"
	TypeResult   = "result"
	TypeEvent    = "event"
	TypeNotify   = "notify"
)

// Command names (§7).
const (
	CmdMachineInfo          = "machine.info"
	CmdEMLyManifestCheck    = "emly.manifest.check"
	CmdUpdaterManifestCheck = "updater.manifest.check"
	CmdAppsListUpgradable   = "apps.list_upgradable"
	CmdServiceRestart       = "service.restart"
	CmdMachineReboot        = "machine.reboot"
)

// Event names (§8).
const (
	EvtMachineInfo     = "machine.info"
	EvtSessionChanged  = "session.changed"
	EvtUpdateAvailable = "update.available"
	EvtUpdateStarted   = "update.started"
	EvtUpdateApplied   = "update.applied"
	EvtUpdateFailed    = "update.failed"
	EvtServiceStarted  = "service.started"
)

// Notify topics (§9).
const (
	TopicReleasePublished = "release.published"
	TopicConfigPublished  = "config.published"
)

// Error codes (§10).
const (
	ErrUnidentified       = "unidentified"
	ErrProtocol           = "protocol_error"
	ErrMessageTooLarge    = "message_too_large"
	ErrRateLimited        = "rate_limited"
	ErrUnsupportedCommand = "unsupported_command"
	ErrInvalidArgs        = "invalid_args"
	ErrExpired            = "expired"
	ErrBusy               = "busy"
	ErrDisabledByPolicy   = "disabled_by_policy"
	ErrInsecureTransport  = "insecure_transport"
	ErrUserActive         = "user_active"
	ErrTimeout            = "timeout"
	ErrInternal           = "internal"
)

// Envelope is every frame on the channel (§3). Only Type is mandatory; the
// v1 bytes {"type":"ping"} are a valid Envelope.
type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	ReplyTo string          `json:"reply_to,omitempty"`
	TS      string          `json:"ts,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// tsLayout is RFC 3339 UTC with milliseconds, the "ts" format of §3.
const tsLayout = "2006-01-02T15:04:05.000Z07:00"

// Frame marshals one message. ping and pong keep the exact v1 bytes (no
// id, no ts); every other type gets a fresh ULID and a timestamp. data nil
// omits "data".
func Frame(typ, replyTo string, data any, now time.Time) ([]byte, error) {
	env := Envelope{Type: typ, ReplyTo: replyTo}
	if typ != TypePing && typ != TypePong {
		env.ID = NewID()
		env.TS = now.UTC().Format(tsLayout)
	}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		env.Data = raw
	}
	return json.Marshal(env)
}

// Hello is hello.data in v2 (§4). A v1 client ignores it.
type Hello struct {
	Protocol      int    `json:"protocol"`
	ServerVersion string `json:"server_version,omitempty"`
	ServerTime    string `json:"server_time"`
}

// IdentityExt is the part of identity.data that v2 adds on top of the v1
// fields (updaterclient.WSIdentityPayload).
type IdentityExt struct {
	Protocol     int      `json:"protocol,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Limits is welcome.data.limits (§11).
type Limits struct {
	MaxMessageBytes    int `json:"max_message_bytes"`
	MaxEventsPerMinute int `json:"max_events_per_minute"`
	AckTimeoutSeconds  int `json:"ack_timeout_seconds"`
}

var DefaultLimits = Limits{MaxMessageBytes: 65536, MaxEventsPerMinute: 60, AckTimeoutSeconds: 5}

type Welcome struct {
	Protocol             int      `json:"protocol"`
	AcceptedCapabilities []string `json:"accepted_capabilities"`
	Limits               Limits   `json:"limits"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type Command struct {
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args,omitempty"`
	ExpiresAt string          `json:"expires_at"`
	IssuedBy  string          `json:"issued_by,omitempty"`
}

type Ack struct {
	Accepted  bool       `json:"accepted"`
	Error     *ErrorBody `json:"error,omitempty"`
	Duplicate bool       `json:"duplicate,omitempty"`
}

// Result statuses.
const (
	ResultOK    = "ok"
	ResultError = "error"
)

type Result struct {
	Status     string          `json:"status"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Error      *ErrorBody      `json:"error,omitempty"`
	Truncated  bool            `json:"truncated,omitempty"`
}

type Event struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Notify struct {
	Topic   string `json:"topic"`
	Payload any    `json:"payload,omitempty"`
}

// UserSession is §6.1.
type UserSession struct {
	User           string `json:"user,omitempty"`
	State          string `json:"state,omitempty"`
	DisconnectedAt string `json:"disconnected_at,omitempty"`
}

// SessionChanged is the payload of event session.changed (§8.1).
type SessionChanged struct {
	Events      []string     `json:"events"`
	SessionID   uint32       `json:"session_id"`
	SessionUser string       `json:"session_user,omitempty"`
	LoggedUser  *UserSession `json:"logged_user,omitempty"`
	Changed     bool         `json:"changed"`
	At          string       `json:"at,omitempty"`
}

// ServiceStarted is the payload of event service.started (§8.6).
type ServiceStarted struct {
	Reason            string   `json:"reason"`
	BootTime          string   `json:"boot_time,omitempty"`
	StartedAt         string   `json:"started_at,omitempty"`
	UpdaterVersion    string   `json:"updater_version,omitempty"`
	PreviousVersion   string   `json:"previous_version,omitempty"`
	CompletedCommands []string `json:"completed_commands,omitempty"`
}

// ReleasePublished / ConfigPublished are the notify payloads of §9.
type ReleasePublished struct {
	Target        string `json:"target"`
	Channel       string `json:"channel,omitempty"`
	Version       string `json:"version"`
	JitterSeconds int    `json:"jitter_seconds"`
}

type ConfigPublished struct {
	Revision      int64 `json:"revision"`
	JitterSeconds int   `json:"jitter_seconds"`
}

// CommandSpec is what the server needs to know about a verb to track it.
type CommandSpec struct {
	// ResultTimeout is measured from the ack. For ResultViaEvent verbs it
	// bounds the wait for service.started instead.
	ResultTimeout time.Duration
	// Destructive verbs need the admin to confirm (dashboard) and a wss://
	// connection on the client (§12.2).
	Destructive bool
	// ResultViaEvent verbs never send a result: the process dies first, and
	// completion is service.started listing the command id (§5.3).
	ResultViaEvent bool
}

var Commands = map[string]CommandSpec{
	CmdMachineInfo:          {ResultTimeout: 30 * time.Second},
	CmdEMLyManifestCheck:    {ResultTimeout: 60 * time.Second},
	CmdUpdaterManifestCheck: {ResultTimeout: 60 * time.Second},
	CmdAppsListUpgradable:   {ResultTimeout: 180 * time.Second},
	CmdServiceRestart:       {ResultTimeout: 120 * time.Second, Destructive: true, ResultViaEvent: true},
	CmdMachineReboot:        {ResultTimeout: 15 * time.Minute, Destructive: true, ResultViaEvent: true},
}

// ServerCapabilities is every command, event and topic this server
// understands; welcome.accepted_capabilities is the client's list
// intersected with it.
var ServerCapabilities = []string{
	CmdMachineInfo, CmdEMLyManifestCheck, CmdUpdaterManifestCheck,
	CmdAppsListUpgradable, CmdServiceRestart, CmdMachineReboot,
	EvtMachineInfo, EvtSessionChanged, EvtUpdateAvailable, EvtUpdateStarted,
	EvtUpdateApplied, EvtUpdateFailed, EvtServiceStarted,
	TopicReleasePublished, TopicConfigPublished,
}

// Intersect returns the entries of offered that are in supported, in
// offered's order, without duplicates.
func Intersect(offered, supported []string) []string {
	ok := make(map[string]bool, len(supported))
	for _, s := range supported {
		ok[s] = true
	}
	out := []string{}
	for _, s := range offered {
		if ok[s] {
			out = append(out, s)
			delete(ok, s)
		}
	}
	return out
}
```

- [ ] **Step 4: Implement `validate.go`**

```go
package clientproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Reboot argument bounds and defaults (§7.6).
const (
	RebootDefaultDelaySeconds = 300
	RebootMaxDelaySeconds     = 3600
	WhenUserActiveWarn        = "warn"
	WhenUserActiveSkip        = "skip"
)

// MachineInfoSections are the allowed values of machine.info's "sections".
var MachineInfoSections = []string{"identity", "logged_user", "network", "site", "config", "hardware", "emly", "update"}

type MachineInfoArgs struct {
	Sections []string `json:"sections,omitempty"`
}

type RebootArgs struct {
	DelaySeconds   *int   `json:"delay_seconds,omitempty"`
	WhenUserActive string `json:"when_user_active,omitempty"`
}

// Delay returns the effective delay, applying the default.
func (a RebootArgs) Delay() int {
	if a.DelaySeconds == nil {
		return RebootDefaultDelaySeconds
	}
	return *a.DelaySeconds
}

// Mode returns the effective when_user_active, applying the default.
func (a RebootArgs) Mode() string {
	if a.WhenUserActive == "" {
		return WhenUserActiveWarn
	}
	return a.WhenUserActive
}

// decodeStrict decodes raw into v rejecting unknown fields. Empty raw is
// "no arguments" and always valid. An argument the verb does not know is an
// error, not a tolerance: an ignored argument is a command run differently
// from how the operator asked (§5.1).
func decodeStrict(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func invalid(format string, args ...any) *ErrorBody {
	return &ErrorBody{Code: ErrInvalidArgs, Message: fmt.Sprintf(format, args...)}
}

// ValidateArgs checks a command's arguments. nil means valid.
func ValidateArgs(name string, raw json.RawMessage) *ErrorBody {
	switch name {
	case CmdMachineInfo:
		var a MachineInfoArgs
		if err := decodeStrict(raw, &a); err != nil {
			return invalid("machine.info: %v", err)
		}
		for _, s := range a.Sections {
			if !contains(MachineInfoSections, s) {
				return invalid("machine.info: unknown section %q", s)
			}
		}
		return nil
	case CmdMachineReboot:
		var a RebootArgs
		if err := decodeStrict(raw, &a); err != nil {
			return invalid("machine.reboot: %v", err)
		}
		if d := a.Delay(); d < 0 || d > RebootMaxDelaySeconds {
			return invalid("machine.reboot: delay_seconds must be between 0 and %d", RebootMaxDelaySeconds)
		}
		if m := a.Mode(); m != WhenUserActiveWarn && m != WhenUserActiveSkip {
			return invalid("machine.reboot: when_user_active must be %q or %q", WhenUserActiveWarn, WhenUserActiveSkip)
		}
		return nil
	case CmdEMLyManifestCheck, CmdUpdaterManifestCheck, CmdAppsListUpgradable, CmdServiceRestart:
		var none struct{}
		if err := decodeStrict(raw, &none); err != nil {
			return invalid("%s takes no arguments: %v", name, err)
		}
		return nil
	default:
		return &ErrorBody{Code: ErrUnsupportedCommand, Message: fmt.Sprintf("unknown command %q", name)}
	}
}

// ResultTimeout is how long after the ack the server waits for the outcome
// of a (valid) command. For machine.reboot it adds the requested delay.
func ResultTimeout(name string, raw json.RawMessage) time.Duration {
	spec := Commands[name]
	if name == CmdMachineReboot {
		var a RebootArgs
		_ = decodeStrict(raw, &a)
		return time.Duration(a.Delay())*time.Second + spec.ResultTimeout
	}
	return spec.ResultTimeout
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/clientproto/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/clientproto
git commit -m "feat(clientproto): wire types, names and argument validation for client ws v2"
```

---

### Task 3: `clientWs.commands` in the remote configuration document

**Files:**
- Modify: `internal/remoteconfig/types.go` (the `ClientWS` field, around line 67)
- Modify: `internal/remoteconfig/parse.go` (`Parse` → call a new `validateClientWS`)
- Modify: `testdata/remoteconfig/valid/full.json`
- Create: `testdata/remoteconfig/invalid/clientws-unknown-command.json`, `testdata/remoteconfig/invalid/clientws-unknown-command.problems.json`
- Test: `internal/remoteconfig/parse_test.go`

**Interfaces:**
- Consumes: `clientproto.Commands` (Task 2) — **no**: `remoteconfig` must stay dependency-free of feature packages; it keeps its own list, `KnownClientWSCommands`, pinned equal to `clientproto.Commands` by a test in `internal/clientws` (Task 6).
- Produces: `type ClientWS struct{Enabled bool; Commands []string}`, `var KnownClientWSCommands []string`.

- [ ] **Step 1: Write the failing tests** (append to `parse_test.go`)

```go
func TestParse_ClientWSCommandsAccepted(t *testing.T) {
	doc, problems := Parse([]byte(`{
		"schemaVersion": 1,
		"servers": {"api": "https://api.example.test"},
		"defaultServer": "api",
		"clientWs": {"enabled": true, "commands": ["machine.info", "machine.reboot"]}
	}`))
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if got := doc.ClientWS.Commands; len(got) != 2 || got[1] != "machine.reboot" {
		t.Fatalf("Commands = %v", got)
	}
}

func TestParse_ClientWSUnknownCommandRejected(t *testing.T) {
	_, problems := Parse([]byte(`{
		"schemaVersion": 1,
		"servers": {"api": "https://api.example.test"},
		"defaultServer": "api",
		"clientWs": {"enabled": true, "commands": ["machine.format_disk"]}
	}`))
	if len(problems) != 1 || problems[0].Path != "/clientWs/commands/0" {
		t.Fatalf("problems = %v, want one at /clientWs/commands/0", problems)
	}
}

// A document that only sets enabled must canonicalize exactly as before
// the commands field existed, or every mirror's ETag changes.
func TestCanonical_ClientWSWithoutCommandsUnchanged(t *testing.T) {
	doc, problems := Parse([]byte(`{
		"schemaVersion": 1,
		"servers": {"api": "https://api.example.test"},
		"defaultServer": "api",
		"clientWs": {"enabled": true}
	}`))
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	b, err := Canonical(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"clientWs":{"enabled":true}`) {
		t.Fatalf("canonical = %s", b)
	}
}
```

Check the minimal document shape against `minimalValidDocJSON` in `parse_test.go`; if `Parse` requires more top-level keys, build these three documents from that constant with the `clientWs` key added instead of the literal above. `Canonical`'s exact signature is in `canonical.go` — adapt the call if it takes a value instead of a pointer.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/remoteconfig/ -run ClientWS -v`
Expected: FAIL — `doc.ClientWS.Commands undefined`.

- [ ] **Step 3: Implement**

In `types.go` replace `ClientWS *ToggleOnly \`json:"clientWs,omitempty"\`` with `ClientWS *ClientWS \`json:"clientWs,omitempty"\`` (keep the long comment, add a paragraph about `commands`), and add:

```go
// ClientWS is the clientWs section: the presence channel's kill switch and,
// from protocol v2, the allowlist of commands the client will execute
// (CLIENT_WS_PROTOCOL.md §12.3). Commands is omitempty for the same ETag
// reason ClientWS itself is: a document that never sets it must canonicalize
// byte-identically to one written before the field existed. Absent means
// the client's own default (read-only commands only), never "all".
type ClientWS struct {
	Enabled  bool     `json:"enabled"`
	Commands []string `json:"commands,omitempty"`
}

// KnownClientWSCommands is every value clientWs.commands may contain. It
// mirrors clientproto.Commands; internal/clientws pins the two equal.
var KnownClientWSCommands = []string{
	"machine.info", "emly.manifest.check", "updater.manifest.check",
	"apps.list_upgradable", "service.restart", "machine.reboot",
}
```

In `parse.go` add, next to the other `validate*` helpers:

```go
func validateClientWS(c *ClientWS) []Problem {
	if c == nil {
		return nil
	}
	var problems []Problem
	for i, name := range c.Commands {
		if !slices.Contains(KnownClientWSCommands, name) {
			problems = append(problems, Problem{
				Path:    fmt.Sprintf("/clientWs/commands/%d", i),
				Message: fmt.Sprintf("unknown command %q", name),
			})
		}
	}
	return problems
}
```

and call it where `Parse` aggregates the other section validators (same place `validateLogging(doc.Logging)` is appended): `problems = append(problems, validateClientWS(doc.ClientWS)...)`. Add `"slices"`/`"fmt"` imports if missing. Fix every compile error from the `*ToggleOnly` → `*ClientWS` change (`grep -rn "ClientWS" internal`; `internal/remoteconfig/parse_test.go` constructs no literal, so only type assertions change).

- [ ] **Step 4: Fixtures (shared with emly-updater)**

`testdata/remoteconfig/valid/full.json`: change the `clientWs` section to

```json
  "clientWs": {
    "enabled": true,
    "commands": ["machine.info", "emly.manifest.check", "updater.manifest.check", "apps.list_upgradable"]
  },
```

`testdata/remoteconfig/invalid/clientws-unknown-command.json` — copy `invalid/unknown-default-server.json`, fix its `defaultServer` so it is valid, and add:

```json
  "clientWs": { "enabled": true, "commands": ["machine.info", "machine.format_disk"] }
```

`testdata/remoteconfig/invalid/clientws-unknown-command.problems.json`:

```json
[
  "/clientWs/commands/1"
]
```

- [ ] **Step 5: Run the whole package**

Run: `go test ./internal/remoteconfig/ ./internal/configapi/ -v`
Expected: PASS, including the fixture-driven tests that walk `testdata/remoteconfig/`.

- [ ] **Step 6: Commit**

```bash
git add internal/remoteconfig testdata/remoteconfig
git commit -m "feat(remoteconfig): clientWs.commands allowlist for protocol v2 commands"
```

---

### Task 4: `updaterclient` — logged-user update from `session.changed`

**Files:**
- Modify: `internal/updaterclient/updaterclient.go`
- Test: `internal/updaterclient/identity_test.go`

**Interfaces:**
- Produces:
  - `func LoggedUserFromWS(uaVersion, user, state, disconnectedAt string) Identity` — only the four logged-user fields (+`NobodyLoggedOn`) are set.
  - `func UpdateLoggedUser(ctx context.Context, db *sqlx.DB, clientID int64, id Identity) error`

- [ ] **Step 1: Write the failing test**

```go
func TestLoggedUserFromWS(t *testing.T) {
	got := LoggedUserFromWS("1.9.0", `CORP\m.rossi`, "disconnected", "2026-09-23T07:58:10Z")
	if got.LoggedUser != `CORP\m.rossi` || got.LoggedUserState != "disconnected" ||
		!got.LoggedUserDisconnectedAt.Equal(time.Date(2026, 9, 23, 7, 58, 10, 0, time.UTC)) || got.NobodyLoggedOn {
		t.Fatalf("got %+v", got)
	}
	// A v2 updater (>= loggedUserSessionMinUpdaterVersion) reporting no
	// user is an answer: nobody is logged on.
	if nobody := LoggedUserFromWS("1.9.0", "", "", ""); !nobody.NobodyLoggedOn {
		t.Fatalf("empty user from 1.9.0 must mean nobody logged on: %+v", nobody)
	}
	// An unknown state is dropped exactly like the header path drops it.
	if odd := LoggedUserFromWS("1.9.0", "u", "sleeping", ""); odd.LoggedUserState != "" {
		t.Fatalf("unknown state kept: %+v", odd)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/updaterclient/ -run LoggedUserFromWS -v`
Expected: FAIL — `undefined: LoggedUserFromWS`.

- [ ] **Step 3: Implement** (after `IdentityFromWSPayload`)

```go
// LoggedUserFromWS builds the logged-user half of an Identity from a
// session.changed event (CLIENT_WS_PROTOCOL.md §8.1), with the same
// conversion rules as the X-EMLy-LoggedUser* headers. uaVersion is the
// updater version from the upgrade request's User-Agent: it decides whether
// an absent user means "nobody" (see reportsNobodyLoggedOn).
func LoggedUserFromWS(uaVersion, user, state, disconnectedAt string) Identity {
	st, at := parseLoggedUserSession(state, disconnectedAt)
	loggedUser := dbvalue.Truncate(user, 255)
	return Identity{
		LoggedUser:               loggedUser,
		LoggedUserState:          st,
		LoggedUserDisconnectedAt: at,
		NobodyLoggedOn:           reportsNobodyLoggedOn(loggedUser, uaVersion),
	}
}

// UpdateLoggedUser writes only the logged-user columns of one client row,
// with the same COALESCE/NULLIF rules as Upsert, and deliberately leaves
// last_seen_at alone: a session event says who is at the machine, not that
// the machine polled.
func UpdateLoggedUser(ctx context.Context, db *sqlx.DB, clientID int64, id Identity) error {
	_, err := db.ExecContext(ctx,
		`UPDATE updater_clients
		 SET logged_user = IF(?, NULL, COALESCE(NULLIF(?, ''), logged_user)),
		     logged_user_disconnected_at = IF(? OR ? <> '', ?, logged_user_disconnected_at),
		     logged_user_state = IF(?, NULL, COALESCE(NULLIF(?, ''), logged_user_state))
		 WHERE id = ?`,
		id.NobodyLoggedOn, id.LoggedUser,
		id.NobodyLoggedOn, id.LoggedUserState, dbvalue.NullTime(id.LoggedUserDisconnectedAt),
		id.NobodyLoggedOn, id.LoggedUserState,
		clientID,
	)
	return err
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/updaterclient/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/updaterclient
git commit -m "feat(updaterclient): update logged user from a client ws session event"
```

---

### Task 5: `internal/clienthub` — sessions, commands, events, notify

**Files:**
- Create: `internal/clienthub/clienthub.go`
- Test: `internal/clienthub/clienthub_test.go`

**Interfaces:**
- Consumes: `clientproto.*` (Task 2).
- Produces (used by Tasks 6–8):

```go
type Sender func(ctx context.Context, frame []byte) error
type Status string // "sent" | "acked" | "rejected" | "done" | "failed" | "timeout"
type CommandRecord struct { ID, Name, IssuedBy string; ClientID int64; Args json.RawMessage; IssuedAt, ExpiresAt time.Time; Status Status; AckedAt, FinishedAt *time.Time; Error *clientproto.ErrorBody; Result *clientproto.Result }
type EventRecord struct { ID, Name string; ClientID int64; ReceivedAt time.Time; ClientTS string; Payload json.RawMessage }
var ErrOffline, ErrUnsupported error
func New(now func() time.Time) *Hub
func (h *Hub) Attach(clientID int64, protocol int, capabilities []string, send Sender) (detach func())
func (h *Hub) Issue(ctx context.Context, clientID int64, name string, args json.RawMessage, ttl time.Duration, issuedBy string) (CommandRecord, error)
func (h *Hub) HandleAck(clientID int64, replyTo string, ack clientproto.Ack)
func (h *Hub) HandleResult(clientID int64, replyTo string, res clientproto.Result)
func (h *Hub) CompleteByRestart(clientID int64, ids []string)
func (h *Hub) Command(id string) (CommandRecord, bool)
func (h *Hub) RecordEvent(ev EventRecord)
func (h *Hub) Events(clientID int64) []EventRecord
func (h *Hub) Notify(ctx context.Context, clientIDs []int64, topic string, payload any) int
func (h *Hub) NotifyConfigPublished(revision int64)
func (h *Hub) Prune()
```

- [ ] **Step 1: Write the failing tests**

```go
package clienthub

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"emly-api-go/internal/clientproto"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type capture struct {
	mu     sync.Mutex
	frames []clientproto.Envelope
}

func (c *capture) send(_ context.Context, frame []byte) error {
	var env clientproto.Envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		return err
	}
	c.mu.Lock()
	c.frames = append(c.frames, env)
	c.mu.Unlock()
	return nil
}

func (c *capture) last(t *testing.T) clientproto.Envelope {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.frames) == 0 {
		t.Fatal("nothing sent")
	}
	return c.frames[len(c.frames)-1]
}

var allCaps = clientproto.ServerCapabilities

func newHub() (*Hub, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)}
	return New(clk.now), clk
}

func TestHubIssueSendsCommand(t *testing.T) {
	h, _ := newHub()
	c := &capture{}
	h.Attach(7, clientproto.ProtocolV2, allCaps, c.send)

	rec, err := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, 10*time.Minute, "admin")
	if err != nil {
		t.Fatal(err)
	}
	env := c.last(t)
	if env.Type != clientproto.TypeCommand || env.ID != rec.ID || rec.Status != StatusSent {
		t.Fatalf("env=%+v rec=%+v", env, rec)
	}
	var cmd clientproto.Command
	_ = json.Unmarshal(env.Data, &cmd)
	if cmd.Name != clientproto.CmdMachineInfo || cmd.ExpiresAt != "2026-09-23T08:10:00Z" || cmd.IssuedBy != "admin" {
		t.Fatalf("cmd = %+v", cmd)
	}
}

func TestHubIssueErrors(t *testing.T) {
	h, _ := newHub()
	ctx := context.Background()
	if _, err := h.Issue(ctx, 1, clientproto.CmdMachineInfo, nil, time.Minute, ""); !errors.Is(err, ErrOffline) {
		t.Errorf("offline: %v", err)
	}
	h.Attach(2, clientproto.ProtocolV1, nil, (&capture{}).send)
	if _, err := h.Issue(ctx, 2, clientproto.CmdMachineInfo, nil, time.Minute, ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("v1 client: %v", err)
	}
	h.Attach(3, clientproto.ProtocolV2, []string{clientproto.CmdMachineInfo}, (&capture{}).send)
	if _, err := h.Issue(ctx, 3, clientproto.CmdMachineReboot, nil, time.Minute, ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("capability not declared: %v", err)
	}
	var nilHub *Hub
	if _, err := nilHub.Issue(ctx, 3, clientproto.CmdMachineInfo, nil, time.Minute, ""); !errors.Is(err, ErrOffline) {
		t.Errorf("nil hub: %v", err)
	}
}

func TestHubAckThenResult(t *testing.T) {
	h, clk := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdAppsListUpgradable, nil, time.Hour, "")

	clk.add(time.Second)
	h.HandleAck(7, rec.ID, clientproto.Ack{Accepted: true})
	if got, _ := h.Command(rec.ID); got.Status != StatusAcked || got.AckedAt == nil {
		t.Fatalf("after ack: %+v", got)
	}
	h.HandleResult(7, rec.ID, clientproto.Result{Status: clientproto.ResultOK, Payload: json.RawMessage(`{"packages":[]}`)})
	got, _ := h.Command(rec.ID)
	if got.Status != StatusDone || got.Result == nil || got.FinishedAt == nil {
		t.Fatalf("after result: %+v", got)
	}
}

func TestHubRejectedAckAndErrorResult(t *testing.T) {
	h, _ := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	a, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineReboot, nil, time.Hour, "")
	h.HandleAck(7, a.ID, clientproto.Ack{Accepted: false, Error: &clientproto.ErrorBody{Code: clientproto.ErrUserActive}})
	if got, _ := h.Command(a.ID); got.Status != StatusRejected || got.Error.Code != clientproto.ErrUserActive {
		t.Fatalf("rejected: %+v", got)
	}
	b, _ := h.Issue(context.Background(), 7, clientproto.CmdEMLyManifestCheck, nil, time.Hour, "")
	h.HandleAck(7, b.ID, clientproto.Ack{Accepted: true})
	h.HandleResult(7, b.ID, clientproto.Result{Status: clientproto.ResultError, Error: &clientproto.ErrorBody{Code: clientproto.ErrBusy}})
	if got, _ := h.Command(b.ID); got.Status != StatusFailed || got.Error.Code != clientproto.ErrBusy {
		t.Fatalf("failed: %+v", got)
	}
}

func TestHubIgnoresAckFromAnotherClient(t *testing.T) {
	h, _ := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	h.Attach(8, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Hour, "")
	h.HandleAck(8, rec.ID, clientproto.Ack{Accepted: true})
	h.HandleResult(8, rec.ID, clientproto.Result{Status: clientproto.ResultOK})
	if got, _ := h.Command(rec.ID); got.Status != StatusSent {
		t.Fatalf("another client's ack/result changed the command: %+v", got)
	}
}

func TestHubAckTimeout(t *testing.T) {
	h, clk := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Hour, "")
	clk.add(ackGrace + time.Second)
	got, _ := h.Command(rec.ID)
	if got.Status != StatusTimeout || got.Error == nil || got.Error.Code != clientproto.ErrTimeout {
		t.Fatalf("got %+v", got)
	}
}

func TestHubResultTimeout(t *testing.T) {
	h, clk := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Hour, "")
	h.HandleAck(7, rec.ID, clientproto.Ack{Accepted: true})
	clk.add(29 * time.Second)
	if got, _ := h.Command(rec.ID); got.Status != StatusAcked {
		t.Fatalf("too early: %+v", got)
	}
	clk.add(2 * time.Second)
	if got, _ := h.Command(rec.ID); got.Status != StatusTimeout {
		t.Fatalf("after 31s: %+v", got)
	}
}

func TestHubCompleteByRestart(t *testing.T) {
	h, clk := newHub()
	detach := h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineReboot,
		json.RawMessage(`{"delay_seconds":60}`), time.Hour, "")
	h.HandleAck(7, rec.ID, clientproto.Ack{Accepted: true})
	detach()
	clk.add(3 * time.Minute)
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	h.CompleteByRestart(7, []string{rec.ID, "unknown-id"})
	if got, _ := h.Command(rec.ID); got.Status != StatusDone {
		t.Fatalf("got %+v", got)
	}
}

func TestHubRebootTimesOutWithoutRestart(t *testing.T) {
	h, clk := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineReboot,
		json.RawMessage(`{"delay_seconds":60}`), time.Hour, "")
	h.HandleAck(7, rec.ID, clientproto.Ack{Accepted: true})
	clk.add(60*time.Second + 15*time.Minute + time.Second)
	if got, _ := h.Command(rec.ID); got.Status != StatusTimeout {
		t.Fatalf("got %+v", got)
	}
}

func TestHubDetachOfSupersededSessionKeepsNewOne(t *testing.T) {
	h, _ := newHub()
	oldDetach := h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	fresh := &capture{}
	h.Attach(7, clientproto.ProtocolV2, allCaps, fresh.send)
	oldDetach()
	if _, err := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Minute, ""); err != nil {
		t.Fatalf("new session lost by old detach: %v", err)
	}
	if fresh.last(t).Type != clientproto.TypeCommand {
		t.Fatal("command did not reach the new session")
	}
}

func TestHubEventsRingKeepsLast50(t *testing.T) {
	h, _ := newHub()
	for i := 0; i < 60; i++ {
		h.RecordEvent(EventRecord{ClientID: 7, Name: clientproto.EvtSessionChanged, ID: clientproto.NewID()})
	}
	h.RecordEvent(EventRecord{ClientID: 8, Name: clientproto.EvtMachineInfo})
	if got := len(h.Events(7)); got != eventRingSize {
		t.Fatalf("len = %d", got)
	}
	if got := len(h.Events(8)); got != 1 {
		t.Fatalf("other client len = %d", got)
	}
}

func TestHubNotifyOnlyV2WithCapability(t *testing.T) {
	h, _ := newHub()
	v2 := &capture{}
	v2NoCap := &capture{}
	v1 := &capture{}
	h.Attach(1, clientproto.ProtocolV2, allCaps, v2.send)
	h.Attach(2, clientproto.ProtocolV2, []string{clientproto.CmdMachineInfo}, v2NoCap.send)
	h.Attach(3, clientproto.ProtocolV1, nil, v1.send)
	n := h.Notify(context.Background(), nil, clientproto.TopicConfigPublished,
		clientproto.ConfigPublished{Revision: 44, JitterSeconds: 120})
	if n != 1 || len(v2.frames) != 1 || len(v2NoCap.frames) != 0 || len(v1.frames) != 0 {
		t.Fatalf("sent=%d v2=%d v2NoCap=%d v1=%d", n, len(v2.frames), len(v2NoCap.frames), len(v1.frames))
	}
	if n := h.Notify(context.Background(), []int64{2, 3}, clientproto.TopicConfigPublished, nil); n != 0 {
		t.Fatalf("targeted notify reached %d clients without the capability", n)
	}
}

func TestHubPruneDropsOldFinishedCommands(t *testing.T) {
	h, clk := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Minute, "")
	h.HandleAck(7, rec.ID, clientproto.Ack{Accepted: true})
	h.HandleResult(7, rec.ID, clientproto.Result{Status: clientproto.ResultOK})
	clk.add(recordRetention + time.Minute)
	h.Prune()
	if _, ok := h.Command(rec.ID); ok {
		t.Fatal("finished record older than retention survived Prune")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/clienthub/ -v`
Expected: FAIL — package does not compile.

- [ ] **Step 3: Implement**

```go
// Package clienthub is the in-memory state of protocol v2 on
// GET /v2/client/ws (CLIENT_WS_PROTOCOL.md): which updater_clients.id has a
// v2 session open and what it can do, the commands issued to machines and
// their outcome, and the last events each machine sent.
//
// HTTP- and DB-free like internal/presencehub, and subject to the same
// single-instance limit: a command issued on one API replica can only reach
// a machine connected to that replica. Nothing survives a restart - command
// history is a debugging aid for now, not an audit trail (the agent-channel
// spec's persistence is the follow-up).
package clienthub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"emly-api-go/internal/clientproto"
)

// Sender writes one frame to a connection.
type Sender func(ctx context.Context, frame []byte) error

type Status string

const (
	StatusSent     Status = "sent"
	StatusAcked    Status = "acked"
	StatusRejected Status = "rejected"
	StatusDone     Status = "done"
	StatusFailed   Status = "failed"
	StatusTimeout  Status = "timeout"
)

const (
	// ackGrace is ack_timeout_seconds plus slack for the round trip.
	ackGrace = time.Duration(clientproto.DefaultLimits.AckTimeoutSeconds)*time.Second + 5*time.Second
	// recordRetention is how long a finished command stays readable.
	recordRetention = 24 * time.Hour
	// eventRingSize is how many events are kept per client.
	eventRingSize = 50
	// sendTimeout bounds one outbound frame.
	sendTimeout = 10 * time.Second
	// configJitterSeconds is the jitter announced with config.published.
	configJitterSeconds = 120
)

var (
	ErrOffline     = errors.New("client is not connected")
	ErrUnsupported = errors.New("client does not support this command")
)

type CommandRecord struct {
	ID         string                 `json:"id"`
	ClientID   int64                  `json:"client_id"`
	Name       string                 `json:"name"`
	Args       json.RawMessage        `json:"args,omitempty"`
	IssuedBy   string                 `json:"issued_by,omitempty"`
	IssuedAt   time.Time              `json:"issued_at"`
	ExpiresAt  time.Time              `json:"expires_at"`
	Status     Status                 `json:"status"`
	AckedAt    *time.Time             `json:"acked_at,omitempty"`
	FinishedAt *time.Time             `json:"finished_at,omitempty"`
	Error      *clientproto.ErrorBody `json:"error,omitempty"`
	Result     *clientproto.Result    `json:"result,omitempty"`

	resultTimeout time.Duration
}

type EventRecord struct {
	ID         string          `json:"id,omitempty"`
	ClientID   int64           `json:"client_id"`
	Name       string          `json:"name"`
	ReceivedAt time.Time       `json:"received_at"`
	ClientTS   string          `json:"client_ts,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

type session struct {
	gen      uint64
	protocol int
	caps     map[string]bool
	send     Sender
}

type Hub struct {
	mu       sync.Mutex
	now      func() time.Time
	nextGen  uint64
	sessions map[int64]*session
	commands map[string]*CommandRecord
	events   map[int64][]EventRecord
}

// New returns an empty Hub; now is time.Now outside tests.
func New(now func() time.Time) *Hub {
	if now == nil {
		now = time.Now
	}
	return &Hub{
		now:      now,
		sessions: map[int64]*session{},
		commands: map[string]*CommandRecord{},
		events:   map[int64][]EventRecord{},
	}
}

// Attach registers the connection now serving clientID, replacing any
// previous one. The returned detach only removes this very session, so a
// superseded connection's cleanup never unregisters its successor (the
// same rule as presencehub.Token).
func (h *Hub) Attach(clientID int64, protocol int, capabilities []string, send Sender) (detach func()) {
	if h == nil {
		return func() {}
	}
	caps := make(map[string]bool, len(capabilities))
	for _, c := range capabilities {
		caps[c] = true
	}
	h.mu.Lock()
	h.nextGen++
	gen := h.nextGen
	h.sessions[clientID] = &session{gen: gen, protocol: protocol, caps: caps, send: send}
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if s, ok := h.sessions[clientID]; ok && s.gen == gen {
			delete(h.sessions, clientID)
		}
	}
}

// Issue validates and sends a command. args must already have passed
// clientproto.ValidateArgs; Issue re-checks it anyway so no caller can skip
// it.
func (h *Hub) Issue(ctx context.Context, clientID int64, name string, args json.RawMessage, ttl time.Duration, issuedBy string) (CommandRecord, error) {
	if h == nil {
		return CommandRecord{}, ErrOffline
	}
	if e := clientproto.ValidateArgs(name, args); e != nil {
		return CommandRecord{}, errors.New(e.Message)
	}
	h.mu.Lock()
	s, ok := h.sessions[clientID]
	h.mu.Unlock()
	if !ok {
		return CommandRecord{}, ErrOffline
	}
	if s.protocol < clientproto.ProtocolV2 || !s.caps[name] {
		return CommandRecord{}, ErrUnsupported
	}

	now := h.now()
	cmd := clientproto.Command{
		Name:      name,
		Args:      args,
		ExpiresAt: now.Add(ttl).UTC().Format(time.RFC3339),
		IssuedBy:  issuedBy,
	}
	frame, err := clientproto.Frame(clientproto.TypeCommand, "", cmd, now)
	if err != nil {
		return CommandRecord{}, err
	}
	var env clientproto.Envelope
	_ = json.Unmarshal(frame, &env)

	rec := &CommandRecord{
		ID: env.ID, ClientID: clientID, Name: name, Args: args, IssuedBy: issuedBy,
		IssuedAt: now, ExpiresAt: now.Add(ttl), Status: StatusSent,
		resultTimeout: clientproto.ResultTimeout(name, args),
	}
	// Registered before the send, so an ack racing the write finds it.
	h.mu.Lock()
	h.commands[rec.ID] = rec
	h.mu.Unlock()

	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := s.send(sctx, frame); err != nil {
		h.mu.Lock()
		delete(h.commands, rec.ID)
		h.mu.Unlock()
		return CommandRecord{}, ErrOffline
	}
	return h.snapshot(rec), nil
}

// ownCommand returns the record replyTo names if it belongs to clientID.
// Caller holds h.mu.
func (h *Hub) ownCommand(clientID int64, replyTo string) (*CommandRecord, bool) {
	rec, ok := h.commands[replyTo]
	if !ok || rec.ClientID != clientID {
		return nil, false
	}
	return rec, true
}

func (h *Hub) HandleAck(clientID int64, replyTo string, ack clientproto.Ack) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.ownCommand(clientID, replyTo)
	if !ok || rec.Status != StatusSent {
		return
	}
	now := h.now()
	if ack.Accepted {
		rec.Status, rec.AckedAt = StatusAcked, &now
		return
	}
	rec.Status, rec.FinishedAt, rec.Error = StatusRejected, &now, ack.Error
}

func (h *Hub) HandleResult(clientID int64, replyTo string, res clientproto.Result) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.ownCommand(clientID, replyTo)
	if !ok || (rec.Status != StatusSent && rec.Status != StatusAcked) {
		return
	}
	now := h.now()
	rec.FinishedAt, rec.Result = &now, &res
	if res.Status == clientproto.ResultOK {
		rec.Status = StatusDone
	} else {
		rec.Status, rec.Error = StatusFailed, res.Error
	}
}

// CompleteByRestart closes the result-via-event commands a service.started
// event lists (§8.6).
func (h *Hub) CompleteByRestart(clientID int64, ids []string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	for _, id := range ids {
		rec, ok := h.ownCommand(clientID, id)
		if !ok || !clientproto.Commands[rec.Name].ResultViaEvent {
			continue
		}
		if rec.Status == StatusSent || rec.Status == StatusAcked || rec.Status == StatusTimeout {
			rec.Status, rec.FinishedAt, rec.Error = StatusDone, &now, nil
		}
	}
}

// Command returns a copy of a record, with timeouts applied lazily: the
// state a reader sees is always current without a sweeper goroutine.
func (h *Hub) Command(id string) (CommandRecord, bool) {
	if h == nil {
		return CommandRecord{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.commands[id]
	if !ok {
		return CommandRecord{}, false
	}
	h.applyTimeout(rec)
	return h.snapshot(rec), true
}

// applyTimeout marks rec timed out when its deadline passed. Caller holds
// h.mu (or owns rec exclusively).
func (h *Hub) applyTimeout(rec *CommandRecord) {
	now := h.now()
	var deadline time.Time
	switch rec.Status {
	case StatusSent:
		deadline = rec.IssuedAt.Add(ackGrace)
	case StatusAcked:
		deadline = rec.AckedAt.Add(rec.resultTimeout)
	default:
		return
	}
	if now.After(deadline) {
		rec.Status, rec.FinishedAt = StatusTimeout, &now
		rec.Error = &clientproto.ErrorBody{Code: clientproto.ErrTimeout}
	}
}

func (h *Hub) snapshot(rec *CommandRecord) CommandRecord {
	c := *rec
	return c
}

func (h *Hub) RecordEvent(ev EventRecord) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ring := append(h.events[ev.ClientID], ev)
	if len(ring) > eventRingSize {
		ring = ring[len(ring)-eventRingSize:]
	}
	h.events[ev.ClientID] = ring
}

// Events returns the client's recent events, oldest first.
func (h *Hub) Events(clientID int64) []EventRecord {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]EventRecord(nil), h.events[clientID]...)
}

// Notify sends a notify to the listed clients (nil = every v2 session)
// that declared the topic, and returns how many it reached.
func (h *Hub) Notify(ctx context.Context, clientIDs []int64, topic string, payload any) int {
	if h == nil {
		return 0
	}
	frame, err := clientproto.Frame(clientproto.TypeNotify, "", clientproto.Notify{Topic: topic, Payload: payload}, h.now())
	if err != nil {
		return 0
	}
	h.mu.Lock()
	var targets []Sender
	pick := func(s *session) {
		if s.protocol >= clientproto.ProtocolV2 && s.caps[topic] {
			targets = append(targets, s.send)
		}
	}
	if clientIDs == nil {
		for _, s := range h.sessions {
			pick(s)
		}
	} else {
		for _, id := range clientIDs {
			if s, ok := h.sessions[id]; ok {
				pick(s)
			}
		}
	}
	h.mu.Unlock()

	sent := 0
	for _, send := range targets {
		sctx, cancel := context.WithTimeout(ctx, sendTimeout)
		if send(sctx, frame) == nil {
			sent++
		}
		cancel()
	}
	return sent
}

// NotifyConfigPublished satisfies configapi.ConfigNotifier.
func (h *Hub) NotifyConfigPublished(revision int64) {
	n := h.Notify(context.Background(), nil, clientproto.TopicConfigPublished,
		clientproto.ConfigPublished{Revision: revision, JitterSeconds: configJitterSeconds})
	slog.Info("client ws: config.published sent", "revision", revision, "clients", n)
}

// Prune drops finished commands older than recordRetention and events of
// clients with nothing newer than that. Called from a ticker in main.
func (h *Hub) Prune() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := h.now().Add(-recordRetention)
	for id, rec := range h.commands {
		h.applyTimeout(rec)
		if rec.FinishedAt != nil && rec.FinishedAt.Before(cutoff) {
			delete(h.commands, id)
		}
	}
	for id, ring := range h.events {
		if len(ring) > 0 && ring[len(ring)-1].ReceivedAt.Before(cutoff) {
			delete(h.events, id)
		}
	}
}
```

Note `TestHubCompleteByRestart` relies on `StatusTimeout` also being completable: the reboot may take longer than the timeout and still come back — `done` wins.

- [ ] **Step 4: Run the tests (with the race detector)**

Run: `go test -race ./internal/clienthub/ -v`
Expected: PASS. (If `-race` is unavailable without cgo on this machine, run without it and note it in the commit body.)

- [ ] **Step 5: Commit**

```bash
git add internal/clienthub
git commit -m "feat(clienthub): in-memory v2 sessions, command lifecycle, events and notify"
```

---

### Task 6: `clientws` handler — v2 handshake, dispatch, limits

**Files:**
- Modify: `internal/clientws/client_ws.route.go`
- Create: `internal/clientws/events.go`
- Modify: `internal/clientws/routes.go` (signature only here; admin routes in Task 7)
- Test: `internal/clientws/client_ws_test.go`

**Interfaces:**
- Consumes: `clientproto` (Task 2), `clienthub.Hub` (Task 5), `updaterclient.LoggedUserFromWS`/`UpdateLoggedUser` (Task 4), `remoteconfig.KnownClientWSCommands` (Task 3).
- Produces: `func ClientWS(db *sqlx.DB, presence *presencehub.Hub, hub *clienthub.Hub) http.HandlerFunc`; `func RegisterV2(r chi.Router, db *sqlx.DB, presence *presencehub.Hub, hub *clienthub.Hub)`; seam `var updateLoggedUserFn = updaterclient.UpdateLoggedUser`.

- [ ] **Step 1: Write the failing tests** (append to `client_ws_test.go`; reuse the existing `upsertClientFn` seam the file already stubs in `TestClientWSIdentityMarksClientOnline` — read that test first and copy its stub pattern)

```go
// dialV2 connects, answers hello with a v2 identity and returns the conn
// and the welcome it got.
func dialV2(t *testing.T, srvURL string, caps []string) (*websocket.Conn, clientproto.Welcome) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srvURL, "http"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	var hello clientproto.Envelope
	readJSON(t, c, &hello)
	var h clientproto.Hello
	if err := json.Unmarshal(hello.Data, &h); err != nil || h.Protocol != clientproto.ProtocolV2 {
		t.Fatalf("hello data = %s", hello.Data)
	}
	writeJSON(t, c, map[string]any{"type": "identity", "id": clientproto.NewID(), "data": map[string]any{
		"hwid": "HW-1", "hostname": "PC-01", "protocol": 2, "capabilities": caps,
	}})
	var env clientproto.Envelope
	readJSON(t, c, &env)
	if env.Type != clientproto.TypeWelcome {
		t.Fatalf("got %s, want welcome", env.Type)
	}
	var w clientproto.Welcome
	_ = json.Unmarshal(env.Data, &w)
	return c, w
}

func readJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
}

func writeJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func stubUpsert(t *testing.T, id int64) {
	t.Helper()
	prev := upsertClientFn
	upsertClientFn = func(context.Context, *sqlx.DB, updaterclient.Identity) (int64, error) { return id, nil }
	t.Cleanup(func() { upsertClientFn = prev })
}

func TestClientWSV2WelcomeIntersectsCapabilities(t *testing.T) {
	stubUpsert(t, 42)
	hub := clienthub.New(nil)
	srv := httptest.NewServer(ClientWS(nil, presencehub.New(time.Second), hub))
	defer srv.Close()

	c, w := dialV2(t, srv.URL, []string{"machine.info", "future.thing"})
	defer c.CloseNow()
	if len(w.AcceptedCapabilities) != 1 || w.AcceptedCapabilities[0] != "machine.info" {
		t.Fatalf("accepted = %v", w.AcceptedCapabilities)
	}
	if w.Limits != clientproto.DefaultLimits {
		t.Fatalf("limits = %+v", w.Limits)
	}
}

func TestClientWSV1ClientGetsNoWelcome(t *testing.T) {
	stubUpsert(t, 42)
	srv := httptest.NewServer(ClientWS(nil, presencehub.New(time.Second), clienthub.New(nil)))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	var hello clientproto.Envelope
	readJSON(t, c, &hello)
	writeJSON(t, c, map[string]any{"type": "identity", "data": map[string]any{"hwid": "HW-1"}})
	// The next frame must be the first ping (10s), not a welcome.
	var next clientproto.Envelope
	rctx, rcancel := context.WithTimeout(ctx, 12*time.Second)
	defer rcancel()
	_, b, err := c.Read(rctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &next)
	if next.Type != clientproto.TypePing || string(b) != `{"type":"ping"}` {
		t.Fatalf("v1 client got %s", b)
	}
}

func TestClientWSCommandRoundTrip(t *testing.T) {
	stubUpsert(t, 42)
	hub := clienthub.New(nil)
	srv := httptest.NewServer(ClientWS(nil, presencehub.New(time.Second), hub))
	defer srv.Close()
	c, _ := dialV2(t, srv.URL, []string{"machine.info"})
	defer c.CloseNow()

	rec, err := hub.Issue(context.Background(), 42, "machine.info", nil, time.Minute, "test")
	if err != nil {
		t.Fatal(err)
	}
	var cmd clientproto.Envelope
	readJSON(t, c, &cmd)
	if cmd.Type != "command" || cmd.ID != rec.ID {
		t.Fatalf("got %+v", cmd)
	}
	writeJSON(t, c, map[string]any{"type": "ack", "id": clientproto.NewID(), "reply_to": rec.ID, "data": map[string]any{"accepted": true}})
	writeJSON(t, c, map[string]any{"type": "result", "id": clientproto.NewID(), "reply_to": rec.ID,
		"data": map[string]any{"status": "ok", "payload": map[string]any{"hostname": "PC-01"}}})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := hub.Command(rec.ID); got.Status == clienthub.StatusDone {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := hub.Command(rec.ID)
	t.Fatalf("command never finished: %+v", got)
}

func TestClientWSSessionChangedUpdatesLoggedUser(t *testing.T) {
	stubUpsert(t, 42)
	type call struct {
		id  int64
		who updaterclient.Identity
	}
	calls := make(chan call, 1)
	prev := updateLoggedUserFn
	updateLoggedUserFn = func(_ context.Context, _ *sqlx.DB, id int64, who updaterclient.Identity) error {
		calls <- call{id, who}
		return nil
	}
	t.Cleanup(func() { updateLoggedUserFn = prev })

	hub := clienthub.New(nil)
	srv := httptest.NewServer(ClientWS(nil, presencehub.New(time.Second), hub))
	defer srv.Close()
	c, _ := dialV2(t, srv.URL, []string{"session.changed"})
	defer c.CloseNow()

	writeJSON(t, c, map[string]any{"type": "event", "id": clientproto.NewID(), "data": map[string]any{
		"name": "session.changed", "payload": map[string]any{
			"events": []string{"logon"}, "session_id": 2, "changed": true,
			"logged_user": map[string]any{"user": `CORP\m.rossi`, "state": "active-console"},
		}}})
	select {
	case got := <-calls:
		if got.id != 42 || got.who.LoggedUser != `CORP\m.rossi` || got.who.LoggedUserState != "active-console" {
			t.Fatalf("got %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UpdateLoggedUser never called")
	}
	if evs := hub.Events(42); len(evs) != 1 || evs[0].Name != "session.changed" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestClientWSEventRateLimit(t *testing.T) {
	stubUpsert(t, 42)
	srv := httptest.NewServer(ClientWS(nil, presencehub.New(time.Second), clienthub.New(nil)))
	defer srv.Close()
	c, _ := dialV2(t, srv.URL, []string{"machine.info"})
	defer c.CloseNow()

	ev := map[string]any{"type": "event", "data": map[string]any{"name": "update.started", "payload": map[string]any{}}}
	for i := 0; i < clientproto.DefaultLimits.MaxEventsPerMinute+1; i++ {
		writeJSON(t, c, ev)
	}
	var env clientproto.Envelope
	readJSON(t, c, &env)
	if env.Type != "error" || !strings.Contains(string(env.Data), clientproto.ErrRateLimited) {
		t.Fatalf("got %s %s", env.Type, env.Data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("close = %v", err)
	}
}

func TestClientWSOversizedMessageClosesConnection(t *testing.T) {
	stubUpsert(t, 42)
	srv := httptest.NewServer(ClientWS(nil, presencehub.New(time.Second), clienthub.New(nil)))
	defer srv.Close()
	c, _ := dialV2(t, srv.URL, []string{"machine.info"})
	defer c.CloseNow()
	big := strings.Repeat("x", clientproto.DefaultLimits.MaxMessageBytes+1)
	writeJSON(t, c, map[string]any{"type": "event", "data": map[string]any{"name": "machine.info", "payload": big}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
				t.Fatalf("close = %v, want 1009", err)
			}
			return
		}
	}
}

// remoteconfig keeps its own list to stay free of feature imports; this
// pins it to the protocol's.
func TestKnownClientWSCommandsMatchProtocol(t *testing.T) {
	if len(remoteconfig.KnownClientWSCommands) != len(clientproto.Commands) {
		t.Fatalf("remoteconfig knows %v, clientproto %d commands", remoteconfig.KnownClientWSCommands, len(clientproto.Commands))
	}
	for _, name := range remoteconfig.KnownClientWSCommands {
		if _, ok := clientproto.Commands[name]; !ok {
			t.Errorf("%s is in remoteconfig but not in clientproto", name)
		}
	}
}
```

Update every existing call `ClientWS(nil, presencehub.New(...))` in the file to `ClientWS(nil, presencehub.New(...), nil)` — a nil hub must keep v1 working (nil-safe methods).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/clientws/ -v`
Expected: FAIL — too many arguments to `ClientWS`.

- [ ] **Step 3: Implement `events.go`**

```go
package clientws

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/clienthub"
	"emly-api-go/internal/clientproto"
	"emly-api-go/internal/updaterclient"
)

// updateLoggedUserFn is the seam tests replace to observe session.changed
// without a database, like upsertClientFn.
var updateLoggedUserFn = updaterclient.UpdateLoggedUser

// handleEvent applies an event's side effects and records it. Unknown
// names are recorded too: the dashboard can show what it cannot interpret.
func handleEvent(ctx context.Context, db *sqlx.DB, hub *clienthub.Hub, clientID int64, uaVersion string, env clientproto.Envelope, ev clientproto.Event) {
	switch ev.Name {
	case clientproto.EvtSessionChanged:
		var p clientproto.SessionChanged
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			slog.DebugContext(ctx, "client ws: bad session.changed payload", "client_id", clientID, "error", err)
			return
		}
		if p.Changed {
			var u clientproto.UserSession
			if p.LoggedUser != nil {
				u = *p.LoggedUser
			}
			who := updaterclient.LoggedUserFromWS(uaVersion, u.User, u.State, u.DisconnectedAt)
			if err := updateLoggedUserFn(ctx, db, clientID, who); err != nil {
				slog.WarnContext(ctx, "client ws: failed to update logged user", "client_id", clientID, "error", err)
			}
		}
	case clientproto.EvtServiceStarted:
		var p clientproto.ServiceStarted
		if err := json.Unmarshal(ev.Payload, &p); err == nil {
			hub.CompleteByRestart(clientID, p.CompletedCommands)
		}
	}
	slog.InfoContext(ctx, "client ws: event", "client_id", clientID, "name", ev.Name)
	hub.RecordEvent(clienthub.EventRecord{
		ID: env.ID, ClientID: clientID, Name: ev.Name, ReceivedAt: timeNow(),
		ClientTS: env.TS, Payload: ev.Payload,
	})
}
```

- [ ] **Step 4: Rework `client_ws.route.go`**

Replace the envelope helpers and the loop with the versions below; keep `readClientIdentity`'s error handling, add the v2 extension.

```go
// timeNow is the clock; a var so tests could pin it.
var timeNow = time.Now

func writeFrame(ctx context.Context, c *websocket.Conn, typ, replyTo string, data any) error {
	b, err := clientproto.Frame(typ, replyTo, data, timeNow())
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, clientWSWriteTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}

func writeClientWSError(ctx context.Context, c *websocket.Conn, code, message string) {
	_ = writeFrame(ctx, c, clientproto.TypeError, "", clientproto.ErrorBody{Code: code, Message: message})
}
```

`readClientIdentity` returns `(updaterclient.Identity, clientproto.IdentityExt, bool)`; after decoding `payload` also do `var ext clientproto.IdentityExt; _ = json.Unmarshal(msg.Data, &ext)`.

The read loop becomes:

```go
// errRateLimited ends the read loop after the rate_limited error was sent.
var errRateLimited = errors.New("rate limited")

// clientWSReadLoop is the connection's single reader (see the v1 comment:
// the idle deadline and "unknown type is ignored" still hold). In v2 it also
// dispatches ack/result to the hub and events to handleEvent.
func clientWSReadLoop(ctx context.Context, c *websocket.Conn, db *sqlx.DB, hub *clienthub.Hub, clientID int64, uaVersion string, v2 bool) error {
	limiter := newEventLimiter(clientproto.DefaultLimits.MaxEventsPerMinute, time.Minute, timeNow)
	for {
		rctx, cancel := context.WithTimeout(ctx, clientWSIdleTimeout)
		_, data, err := c.Read(rctx)
		cancel()
		if err != nil {
			return err
		}
		var env clientproto.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		switch {
		case env.Type == clientproto.TypePong:
		case v2 && env.Type == clientproto.TypeAck:
			var ack clientproto.Ack
			if json.Unmarshal(env.Data, &ack) == nil {
				hub.HandleAck(clientID, env.ReplyTo, ack)
			}
		case v2 && env.Type == clientproto.TypeResult:
			var res clientproto.Result
			if json.Unmarshal(env.Data, &res) == nil {
				hub.HandleResult(clientID, env.ReplyTo, res)
			}
		case v2 && env.Type == clientproto.TypeEvent:
			if !limiter.allow() {
				writeClientWSError(ctx, c, clientproto.ErrRateLimited, "too many events")
				return errRateLimited
			}
			var ev clientproto.Event
			if json.Unmarshal(env.Data, &ev) == nil {
				handleEvent(ctx, db, hub, clientID, uaVersion, env, ev)
			}
		default:
			slog.DebugContext(ctx, "client ws: unrecognized message type", "type", env.Type)
		}
	}
}

// eventLimiter is a fixed-window counter: at most max events per window.
type eventLimiter struct {
	max    int
	window time.Duration
	now    func() time.Time
	start  time.Time
	count  int
}

func newEventLimiter(max int, window time.Duration, now func() time.Time) *eventLimiter {
	return &eventLimiter{max: max, window: window, now: now, start: now()}
}

func (l *eventLimiter) allow() bool {
	if t := l.now(); t.Sub(l.start) >= l.window {
		l.start, l.count = t, 0
	}
	l.count++
	return l.count <= l.max
}
```

The ping loop writes with `writeFrame(ctx, c, clientproto.TypePing, "", nil)` (bytes unchanged: `{"type":"ping"}`).

`ClientWS(db, presence, hub)`:

1. After `websocket.Accept`, call `c.SetReadLimit(int64(clientproto.DefaultLimits.MaxMessageBytes))`. coder/websocket closes with 1009 (`StatusMessageTooBig`) by itself when exceeded.
2. Send hello as `writeFrame(ctx, c, clientproto.TypeHello, "", clientproto.Hello{Protocol: clientproto.ProtocolCurrent, ServerTime: timeNow().UTC().Format(time.RFC3339)})`. `hello` gets an `id`/`ts` too; the v1 updater only reads `type`.
3. After upsert and `presence.Connect`: `v2 := ext.Protocol >= clientproto.ProtocolV2`. If `v2`:
   ```go
   accepted := clientproto.Intersect(ext.Capabilities, clientproto.ServerCapabilities)
   if err := writeFrame(ctx, c, clientproto.TypeWelcome, "", clientproto.Welcome{
       Protocol: clientproto.ProtocolV2, AcceptedCapabilities: accepted, Limits: clientproto.DefaultLimits,
   }); err != nil { /* same teardown as an upsert failure */ }
   detach := hub.Attach(clientID, clientproto.ProtocolV2, accepted, func(ctx context.Context, frame []byte) error {
       return c.Write(ctx, websocket.MessageText, frame)
   })
   defer detach()
   ```
   A v1 client is attached too, with `ProtocolV1` and no capabilities, so `Issue` answers `ErrUnsupported` rather than `ErrOffline` for it — the admin sees the real reason.
4. `err := clientWSReadLoop(ctx, c, db, hub, clientID, identity.UAVersion, v2)`; after `cancel(); wg.Wait(); presence.Disconnect(tok)`:
   - `errors.Is(err, errRateLimited)` → `c.Close(websocket.StatusPolicyViolation, "rate limited")`, return;
   - superseded → unchanged;
   - otherwise unchanged normal closure.

- [ ] **Step 5: Update `routes.go` signature**

```go
func RegisterV2(r chi.Router, db *sqlx.DB, presence *presencehub.Hub, hub *clienthub.Hub) {
	r.Route("/client", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))
			r.Get("/ws", ClientWS(db, presence, hub))
		})
	})
}
```

Fix the callers so it compiles: `internal/routes/v2/v2.go` passes `nil` for now (Task 8 threads the real hub), `main.go` line ~308 `clientws.ClientWS(db, presenceHub, nil)` for now.

- [ ] **Step 6: Run the tests**

Run: `go build ./... && go test ./internal/clientws/ ./internal/routes/... -v`
Expected: PASS. `TestClientWSV1ClientGetsNoWelcome` takes ~10s (first ping).

- [ ] **Step 7: Commit**

```bash
git add internal/clientws internal/routes main.go
git commit -m "feat(clientws): protocol v2 handshake, command/event dispatch and limits"
```

---

### Task 7: Admin REST routes for commands, events and notify

**Files:**
- Create: `internal/clientws/admin.route.go`
- Modify: `internal/clientws/routes.go`
- Test: `internal/clientws/admin_test.go`, `internal/routes/v2/client_routing_test.go`

**Interfaces:**
- Consumes: `clienthub.Hub` (Task 5), `clientproto.ValidateArgs` (Task 2).
- Produces: `IssueCommand(hub)`, `GetCommand(hub)`, `ListEvents(hub)`, `SendNotify(hub)` handlers; routes
  - `POST /v2/client/{client_id}/commands` (ADMIN)
  - `GET /v2/client/commands/{command_id}` (ADMIN)
  - `GET /v2/client/{client_id}/events` (ADMIN)
  - `POST /v2/client/notify` (ADMIN)

Request/response contract:

| Route | Body | Success | Errors |
|---|---|---|---|
| `POST /{client_id}/commands` | `{"name":"…","args":{…},"ttl_seconds":600,"issued_by":"…"}` | `202` + `CommandRecord` | `400` bad JSON / bad `client_id` / `invalid_args` / ttl out of `1..86400`; `409` offline; `422` unsupported (v1 client or capability not declared, or unknown name) |
| `GET /commands/{command_id}` | — | `200` + `CommandRecord` | `404` |
| `GET /{client_id}/events` | — | `200` + `{"events":[…]}` (oldest first) | `400` bad id |
| `POST /notify` | `{"topic":"release.published","payload":{…},"client_ids":[1,2]}` | `200` + `{"sent":N}` | `400` unknown topic / invalid payload |

`notify` payload rules: `release.published` requires `target` ∈ {`emly`,`updater`}, non-empty `version`, `jitter_seconds` ≥ 60 (below 60 → 400: the client enforces 60 anyway, the API refuses to pretend otherwise); `channel` required when `target` is `emly` (`stable`/`beta`). `config.published` requires `revision` > 0 and `jitter_seconds` ≥ 0. `client_ids` omitted = every connected v2 client.

- [ ] **Step 1: Write the failing tests** (`admin_test.go`)

```go
package clientws

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"emly-api-go/internal/clienthub"
	"emly-api-go/internal/clientproto"
)

func adminRouter(hub *clienthub.Hub) http.Handler {
	r := chi.NewRouter()
	mountAdmin(r, hub)
	return r
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type sink struct{ frames [][]byte }

func (s *sink) send(_ context.Context, b []byte) error { s.frames = append(s.frames, b); return nil }

func TestIssueCommandAccepted(t *testing.T) {
	hub := clienthub.New(nil)
	s := &sink{}
	hub.Attach(42, clientproto.ProtocolV2, clientproto.ServerCapabilities, s.send)
	rec := do(t, adminRouter(hub), "POST", "/42/commands", `{"name":"machine.info","issued_by":"f.fois"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
	var got clienthub.CommandRecord
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Status != clienthub.StatusSent || got.ClientID != 42 || len(s.frames) != 1 {
		t.Fatalf("got %+v frames=%d", got, len(s.frames))
	}
	if d := got.ExpiresAt.Sub(got.IssuedAt); d != 600*time.Second {
		t.Fatalf("default ttl = %v", d)
	}
}

func TestIssueCommandErrors(t *testing.T) {
	hub := clienthub.New(nil)
	hub.Attach(1, clientproto.ProtocolV1, nil, (&sink{}).send)
	r := adminRouter(hub)
	cases := []struct {
		path, body string
		want       int
	}{
		{"/abc/commands", `{"name":"machine.info"}`, 400},
		{"/42/commands", `{`, 400},
		{"/42/commands", `{"name":"machine.reboot","args":{"delay_seconds":9999}}`, 400},
		{"/42/commands", `{"name":"machine.info","ttl_seconds":90000}`, 400},
		{"/42/commands", `{"name":"machine.format_disk"}`, 422},
		{"/42/commands", `{"name":"machine.info"}`, 409},
		{"/1/commands", `{"name":"machine.info"}`, 422},
	}
	for _, c := range cases {
		if rec := do(t, r, "POST", c.path, c.body); rec.Code != c.want {
			t.Errorf("POST %s %s = %d (%s), want %d", c.path, c.body, rec.Code, rec.Body, c.want)
		}
	}
}

func TestGetCommand(t *testing.T) {
	hub := clienthub.New(nil)
	hub.Attach(42, clientproto.ProtocolV2, clientproto.ServerCapabilities, (&sink{}).send)
	issued, _ := hub.Issue(context.Background(), 42, "machine.info", nil, time.Minute, "")
	r := adminRouter(hub)
	if rec := do(t, r, "GET", "/commands/"+issued.ID, ""); rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if rec := do(t, r, "GET", "/commands/nope", ""); rec.Code != 404 {
		t.Fatalf("missing = %d", rec.Code)
	}
}

func TestListEvents(t *testing.T) {
	hub := clienthub.New(nil)
	hub.RecordEvent(clienthub.EventRecord{ClientID: 42, Name: "update.applied"})
	rec := do(t, adminRouter(hub), "GET", "/42/events", "")
	var body struct{ Events []clienthub.EventRecord }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || len(body.Events) != 1 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestSendNotify(t *testing.T) {
	hub := clienthub.New(nil)
	s := &sink{}
	hub.Attach(42, clientproto.ProtocolV2, clientproto.ServerCapabilities, s.send)
	r := adminRouter(hub)
	ok := `{"topic":"release.published","payload":{"target":"emly","channel":"stable","version":"3.5.0","jitter_seconds":600}}`
	if rec := do(t, r, "POST", "/notify", ok); rec.Code != 200 || rec.Body.String() != "{\"sent\":1}\n" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body)
	}
	for _, bad := range []string{
		`{"topic":"weather"}`,
		`{"topic":"release.published","payload":{"target":"emly","channel":"stable","version":"3.5.0","jitter_seconds":10}}`,
		`{"topic":"release.published","payload":{"target":"emly","version":"3.5.0","jitter_seconds":600}}`,
		`{"topic":"release.published","payload":{"target":"office","version":"1","jitter_seconds":600}}`,
		`{"topic":"config.published","payload":{"revision":0,"jitter_seconds":60}}`,
	} {
		if rec := do(t, r, "POST", "/notify", bad); rec.Code != 400 {
			t.Errorf("%s = %d, want 400", bad, rec.Code)
		}
	}
}
```

In `internal/routes/v2/client_routing_test.go` add, in the same style as the existing test there:

```go
func TestClientAdminRoutesRequireAdminKey(t *testing.T) {
	r := NewRouter(nil, nil, nil, nil, nil, nil, nil, nil)
	for _, c := range []struct{ method, path string }{
		{"POST", "/client/42/commands"},
		{"GET", "/client/commands/01J8ZQ6T3M6X9K2V7B4N1C5D8E"},
		{"GET", "/client/42/events"},
		{"POST", "/client/notify"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without X-Admin-Key = %d, want 401", c.method, c.path, rec.Code)
		}
	}
}
```

(The `NewRouter` argument count must match its signature after Task 8 adds the hub; write the test with Task 8's signature and it will compile once Task 8 lands — or run Task 8 Step 1 first. Check how the existing routing tests build the router and match them exactly; `AdminKeyAuth(nil)` must answer 401 without a DB, which is what the existing admin routing tests already rely on.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/clientws/ -run "Command|Events|Notify" -v`
Expected: FAIL — `undefined: mountAdmin`.

- [ ] **Step 3: Implement `admin.route.go`**

```go
package clientws

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"emly-api-go/internal/clienthub"
	"emly-api-go/internal/clientproto"
	"emly-api-go/internal/response"
)

const (
	defaultCommandTTL = 600 * time.Second
	maxCommandTTL     = 86400 * time.Second
	minReleaseJitter  = 60
)

// mountAdmin mounts the admin half of the client channel on r (already
// scoped to /v2/client and behind AdminKeyAuth by RegisterV2). Split out so
// the handler tests can mount it without auth.
func mountAdmin(r chi.Router, hub *clienthub.Hub) {
	r.Post("/{client_id}/commands", IssueCommand(hub))
	r.Get("/commands/{command_id}", GetCommand(hub))
	r.Get("/{client_id}/events", ListEvents(hub))
	r.Post("/notify", SendNotify(hub))
}

func clientIDParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "client_id"), 10, 64)
	return id, err == nil && id > 0
}

type issueRequest struct {
	Name       string          `json:"name"`
	Args       json.RawMessage `json:"args"`
	TTLSeconds int             `json:"ttl_seconds"`
	IssuedBy   string          `json:"issued_by"`
}

// IssueCommand handles POST /v2/client/{client_id}/commands.
func IssueCommand(hub *clienthub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID, ok := clientIDParam(r)
		if !ok {
			response.Error(w, http.StatusBadRequest, "invalid client_id")
			return
		}
		var req issueRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		ttl := defaultCommandTTL
		if req.TTLSeconds != 0 {
			ttl = time.Duration(req.TTLSeconds) * time.Second
		}
		if ttl <= 0 || ttl > maxCommandTTL {
			response.Error(w, http.StatusBadRequest, "ttl_seconds must be between 1 and 86400")
			return
		}
		if e := clientproto.ValidateArgs(req.Name, req.Args); e != nil {
			status := http.StatusBadRequest
			if e.Code == clientproto.ErrUnsupportedCommand {
				status = http.StatusUnprocessableEntity
			}
			response.Error(w, status, e.Message)
			return
		}
		rec, err := hub.Issue(r.Context(), clientID, req.Name, req.Args, ttl, req.IssuedBy)
		switch {
		case errors.Is(err, clienthub.ErrOffline):
			response.Error(w, http.StatusConflict, err.Error())
		case errors.Is(err, clienthub.ErrUnsupported):
			response.Error(w, http.StatusUnprocessableEntity, err.Error())
		case err != nil:
			response.Error(w, http.StatusInternalServerError, "failed to send command")
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(rec)
		}
	}
}

// GetCommand handles GET /v2/client/commands/{command_id}.
func GetCommand(hub *clienthub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec, ok := hub.Command(chi.URLParam(r, "command_id"))
		if !ok {
			response.Error(w, http.StatusNotFound, "command not found")
			return
		}
		response.OK(w, rec)
	}
}

// ListEvents handles GET /v2/client/{client_id}/events.
func ListEvents(hub *clienthub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID, ok := clientIDParam(r)
		if !ok {
			response.Error(w, http.StatusBadRequest, "invalid client_id")
			return
		}
		events := hub.Events(clientID)
		if events == nil {
			events = []clienthub.EventRecord{}
		}
		response.OK(w, map[string]any{"events": events})
	}
}

type notifyRequest struct {
	Topic     string          `json:"topic"`
	Payload   json.RawMessage `json:"payload"`
	ClientIDs []int64         `json:"client_ids"`
}

// SendNotify handles POST /v2/client/notify.
func SendNotify(hub *clienthub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req notifyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		payload, msg := validateNotify(req.Topic, req.Payload)
		if msg != "" {
			response.Error(w, http.StatusBadRequest, msg)
			return
		}
		sent := hub.Notify(r.Context(), req.ClientIDs, req.Topic, payload)
		response.OK(w, map[string]int{"sent": sent})
	}
}

func validateNotify(topic string, raw json.RawMessage) (any, string) {
	switch topic {
	case clientproto.TopicReleasePublished:
		var p clientproto.ReleasePublished
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, "invalid release.published payload"
		}
		switch {
		case p.Target != "emly" && p.Target != "updater":
			return nil, `target must be "emly" or "updater"`
		case p.Version == "":
			return nil, "version is required"
		case p.Target == "emly" && p.Channel != "stable" && p.Channel != "beta":
			return nil, `channel must be "stable" or "beta" for target emly`
		case p.JitterSeconds < minReleaseJitter:
			return nil, "jitter_seconds must be >= 60"
		}
		return p, ""
	case clientproto.TopicConfigPublished:
		var p clientproto.ConfigPublished
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, "invalid config.published payload"
		}
		if p.Revision <= 0 || p.JitterSeconds < 0 {
			return nil, "revision must be > 0 and jitter_seconds >= 0"
		}
		return p, ""
	default:
		return nil, "unknown topic"
	}
}
```

- [ ] **Step 4: Mount in `routes.go`** — inside `r.Route("/client", ...)`, after the existing API-key group:

```go
		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))
			mountAdmin(r, hub)
		})
```

chi matches `/ws` (static) before `/{client_id}/...`, and `/commands/{id}` / `/notify` are distinct from `/{client_id}/commands`, so there is no conflict; the routing test proves it.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/clientws/ ./internal/routes/... -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/clientws internal/routes
git commit -m "feat(clientws): admin routes to issue commands, read outcomes and events, send notify"
```

---

### Task 8: Wiring — hub in main, routers, config publish notify

**Files:**
- Modify: `internal/routes/v2/v2.go`, `internal/routes/routes.go`, `main.go`
- Modify: `internal/configapi/routes.go`, `internal/configapi/config.route.go`
- Test: `internal/configapi/notify_test.go` (new), existing routing tests

**Interfaces:**
- Consumes: `clienthub.Hub.NotifyConfigPublished` (Task 5).
- Produces: `configapi.ConfigNotifier interface{ NotifyConfigPublished(revision int64) }`; `configapi.RegisterV2(r, db, cfg, notifier ConfigNotifier)`; `v2.NewRouter(..., presence *presencehub.Hub, clients *clienthub.Hub, reloader bans.BanReloader)`; `routes.RegisterAll(..., presence, clients, bans)`.

- [ ] **Step 1: Thread the hub** — add a `clients *clienthub.Hub` parameter right after `presence` in `v2.NewRouter` and `routes.RegisterAll`, pass it to `clientws.RegisterV2(r, db, presence, clients)` and `configapi.RegisterV2(r, db, config.Load(), clients)`. Update every caller (`go build ./...` lists them; the routing tests pass `nil`). In `main.go`, next to `presenceHub := presencehub.New(...)`:

```go
	// clientHub holds protocol v2 of GET /v2/client/ws: sessions, commands
	// issued to machines and their recent events (internal/clienthub).
	// In-memory and single-instance, like presenceHub.
	clientHub := clienthub.New(time.Now)
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			clientHub.Prune()
		}
	}()
```

pass `clientHub` to `routes.RegisterAll` and to `clientws.ClientWS(db, presenceHub, clientHub)` in the hijack-safe `mux` wiring. (If `main.go` already has a shutdown context the other background goroutines select on, use it instead of a bare `for range`.)

- [ ] **Step 2: Write the failing notifier test** (`internal/configapi/notify_test.go`)

```go
package configapi

import "testing"

type recordingNotifier struct{ revisions []int64 }

func (r *recordingNotifier) NotifyConfigPublished(rev int64) { r.revisions = append(r.revisions, rev) }

func TestNotifyPublishedToleratesNil(t *testing.T) {
	notifyPublished(nil, 5) // must not panic
}

func TestNotifyPublishedForwards(t *testing.T) {
	r := &recordingNotifier{}
	notifyPublished(r, 44)
	if len(r.revisions) != 1 || r.revisions[0] != 44 {
		t.Fatalf("got %v", r.revisions)
	}
}
```

Run: `go test ./internal/configapi/ -run NotifyPublished -v` — Expected: FAIL (`undefined: notifyPublished`).

- [ ] **Step 3: Implement** in `config.route.go`

```go
// ConfigNotifier is told when a revision becomes the published one, so the
// machines holding a v2 client channel re-fetch GET /v2/config now instead
// of at their next refresh (CLIENT_WS_PROTOCOL.md §9.2). The notify is only
// a hint: the machine still validates what it downloads.
type ConfigNotifier interface {
	NotifyConfigPublished(revision int64)
}

// notifyPublished tolerates a nil notifier so tests need no special case.
// A typed-nil *clienthub.Hub is also safe: its methods are nil-receiver safe.
func notifyPublished(n ConfigNotifier, revision int64) {
	if n == nil {
		return
	}
	n.NotifyConfigPublished(revision)
}
```

Beware passing a nil `*clienthub.Hub` as `ConfigNotifier`: the interface is then non-nil, which is fine only because `(*Hub).NotifyConfigPublished` → `Notify` returns 0 on a nil receiver. Keep it that way.

Give `CreateConfigRevision`, `PublishConfigRevision` and `RollbackConfig` a `notifier ConfigNotifier` parameter and call `notifyPublished(notifier, revision)` right after each of the `slog.InfoContext(..., "config revision published", ...)` lines (for `RollbackConfig`, right after its successful commit, with the revision that is now published). `RegisterV2(r, db, cfg, notifier)` passes it through.

- [ ] **Step 4: Build and run everything**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go internal/routes internal/configapi
git commit -m "feat: wire clienthub into the API and announce config publishes over the client channel"
```

---

### Task 9: Documentation

**Files:**
- Modify: `CLIENT_WS_PROTOCOL.md`, `ROUTES.md`, `DOCS.md`, `CLAUDE.md`, `.github/copilot-instructions.md`

- [ ] **Step 1: `CLIENT_WS_PROTOCOL.md`**
  - Header "Stato": v2 implemented on the API side (this branch); updater side per its own plan.
  - New section "Superficie REST (admin)" with the table from Task 7 (routes, bodies, codes).
  - §12.3: add that in this implementation the **server does not** filter `accepted_capabilities` by `clientWs.commands` (it cannot evaluate DC/subnet overrides for a host it only knows by HWID/hostname); the client enforces the allowlist and answers `disabled_by_policy`.
  - §8.1: note `updater_clients.logged_user*` is updated from `session.changed` without touching `last_seen_at`.
  - §7.2/§7.3: the dry-run checks are read-only (no download, no `state.json`), so the updater runs them even while a `Cycle` is in progress instead of answering `busy`; `busy` is only for the same command already running. Drop "Se un `Cycle` è in corso risponde `busy`".

- [ ] **Step 2: `ROUTES.md`** — §5.9: add the four admin routes to the table (`ADMIN`), the body/status tables, and a sentence that state is in memory (lost on restart, single instance). Update §6 "Riepilogo autenticazione" (`X-Admin-Key` row).

- [ ] **Step 3: `DOCS.md`** (Italian, Node/PHP analogies) — in "Presenza client in tempo reale": a subsection on `internal/clientproto` (like a shared TypeScript types package between client and server), `internal/clienthub` (like a `Map` of socket.io rooms kept in process memory, with the same single-instance caveat as `presencehub`), the command lifecycle `sent → acked → done|failed|rejected|timeout`, and why `service.restart`/`machine.reboot` finish via `service.started`. Add both packages to the directory tree near line 94.

- [ ] **Step 4: `CLAUDE.md` + `.github/copilot-instructions.md`** — package list (`internal/clientproto`, `internal/clienthub`), the test-coverage comment at the top (clientproto IDs/validation, clienthub lifecycle, v2 handshake/dispatch/limits), and replace the "only hello/identity/ping/pong exist" convention with: v2 adds welcome/command/ack/result/event/notify; unknown `type` still ignored; unknown command name refused; `CLIENT_WS_PROTOCOL.md` is the normative wire format, mirrored by hand in `emly-updater/internal/wsclient`.

- [ ] **Step 5: Final verification and commit**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

```bash
git add CLIENT_WS_PROTOCOL.md ROUTES.md DOCS.md CLAUDE.md .github/copilot-instructions.md
git commit -m "docs: document client ws protocol v2 implementation and admin routes"
```
