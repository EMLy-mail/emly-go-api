package clientws

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/clienthub"
	"emly-api-go/internal/clientproto"
	"emly-api-go/internal/presencehub"
	"emly-api-go/internal/remoteconfig"
	"emly-api-go/internal/updaterclient"
)

func TestClientIdentityFromWSPayload(t *testing.T) {
	r := httptest.NewRequest("GET", "/v2/client/ws", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("User-Agent", "EMLy-Updater/1.6.3 (f.fois@3git.eu)")

	payload := updaterclient.WSIdentityPayload{
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

	got := updaterclient.IdentityFromWSPayload(r, payload)
	want := updaterclient.Identity{
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
	got := updaterclient.IdentityFromWSPayload(r, updaterclient.WSIdentityPayload{})
	if got.Identified() {
		t.Fatalf("an empty payload must not be identified()")
	}
}

// TestClientWSSendsHelloOnConnect checks the first half of the handshake
// (design doc §3.1): the server speaks first. It never sends an identity
// back, so the handler never reaches updaterclient.Upsert - nil db is safe.
func TestClientWSSendsHelloOnConnect(t *testing.T) {
	srv := httptest.NewServer(ClientWS(nil, presencehub.New(presencehub.DefaultGraceDuration), nil))
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
	var env clientWSEnvelope
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
// and never reaches updaterclient.Upsert, so nil db is safe here too.
func TestClientWSRejectsNonIdentityFirstMessage(t *testing.T) {
	srv := httptest.NewServer(ClientWS(nil, presencehub.New(presencehub.DefaultGraceDuration), nil))
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
	var env clientWSEnvelope
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
// §3.1's "non identificato"). Also never reaches updaterclient.Upsert.
func TestClientWSRejectsUnidentifiedIdentity(t *testing.T) {
	srv := httptest.NewServer(ClientWS(nil, presencehub.New(presencehub.DefaultGraceDuration), nil))
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
	var env clientWSEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	if env.Type != "error" {
		t.Fatalf("response type = %q, want %q", env.Type, "error")
	}
}

// waitFor polls cond every 20ms until it returns true or timeout elapses,
// failing the test if it never does. Used below instead of a fixed Sleep so
// the test is fast on a healthy run and not flaky on a slow one.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// TestClientWSIdentityMarksClientOnline drives the feature's actual promise -
// "a client that completes the handshake shows up online" - end to end over
// a real listener and real dial: identity -> upsert (stubbed via
// upsertClientFn) -> presencehub.Connect -> presence.Online reports true,
// then a clean client-side close -> presencehub.Disconnect (after its grace
// period) -> presence.Online reports false again. Every other test in this
// file stops before updaterclient.Upsert (nil db) or never drives the
// handshake at all; this is the one that closes the gap.
func TestClientWSIdentityMarksClientOnline(t *testing.T) {
	const fakeClientID = int64(424242)

	orig := upsertClientFn
	upsertClientFn = func(ctx context.Context, db *sqlx.DB, id updaterclient.Identity) (int64, error) {
		return fakeClientID, nil
	}
	t.Cleanup(func() { upsertClientFn = orig })

	// Short grace period (like presencehub's own tests) so the offline half
	// of this test doesn't need to sleep for the real 15s default.
	presence := presencehub.New(50 * time.Millisecond)

	srv := httptest.NewServer(ClientWS(nil, presence, nil))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	if _, _, err := c.Read(ctx); err != nil { // consume "hello"
		t.Fatalf("Read hello: %v", err)
	}

	if presence.Online(fakeClientID) {
		t.Fatal("client reported online before the identity message was even sent")
	}

	identityMsg := `{"type":"identity","data":{"hwid":"TEST-HWID-CLIENTWS-001"}}`
	if err := c.Write(ctx, websocket.MessageText, []byte(identityMsg)); err != nil {
		t.Fatalf("Write identity: %v", err)
	}

	waitFor(t, 2*time.Second, "presence.Online(fakeClientID) to become true after identity", func() bool {
		return presence.Online(fakeClientID)
	})

	// Confirm the connection is still open past the handshake: a read
	// bounded well under the 10s ping interval must time out (context
	// deadline), not report a close.
	readCtx, readCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, _, err = c.Read(readCtx)
	readCancel()
	if err == nil {
		t.Fatal("unexpected message read; expected the bounded read to time out")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the connection to still be open (context deadline exceeded), got: %v", err)
	}

	if !presence.Online(fakeClientID) {
		t.Fatal("client must still be online while the connection is held open")
	}

	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("Close: %v", err)
	}

	waitFor(t, 2*time.Second, "presence.Online(fakeClientID) to become false after close + grace period", func() bool {
		return !presence.Online(fakeClientID)
	})
}

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
