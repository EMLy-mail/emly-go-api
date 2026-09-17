package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"emly-api-go/internal/presencehub"
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
