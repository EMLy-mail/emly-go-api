package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"emly-api-go/internal/statshub"
)

// TestMain seeds the env the config singleton requires so StatsStream's
// admin-key check can be built without a live database, same convention as
// internal/routes/v2's TestMain.
func TestMain(m *testing.M) {
	os.Setenv("DATABASE_NAME", "emly_test")
	os.Setenv("DB_DSN", "root:secret@tcp(127.0.0.1:3306)/emly_test?parseTime=true&loc=UTC")
	os.Setenv("API_KEY", "test-api-key")
	os.Setenv("ADMIN_KEY", "test-admin-key")
	os.Exit(m.Run())
}

// TestStatsStreamRejectsMissingOrWrongAdminKey pins down design doc §4: the
// admin key is checked before any WebSocket upgrade is attempted, so a bad
// key gets a plain 401 the caller can tell apart from a network problem -
// never an accepted-then-closed connection.
func TestStatsStreamRejectsMissingOrWrongAdminKey(t *testing.T) {
	h := StatsStream(nil, statshub.New())

	cases := []struct {
		name    string
		headers map[string]string
		query   string
	}{
		{name: "no key at all"},
		{name: "wrong header key", headers: map[string]string{"X-Admin-Key": "nope"}},
		{name: "wrong query key", query: "?admin_key=nope"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/stream"+tc.query, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()

			h(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusUnauthorized, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

// TestStatsStreamAcceptsQueryStringKeyFallback checks the proxy-strips-
// headers fallback from design doc §4: an admin key passed as ?admin_key=
// is accepted exactly like the X-Admin-Key header. It only needs to observe
// that this request gets *past* the auth check (a real upgrade attempt
// follows, which httptest.NewRecorder can't complete since it isn't a
// Hijacker) - see TestStatsStreamPingPong for a full handshake over a real
// listener.
func TestStatsStreamAcceptsQueryStringKeyFallback(t *testing.T) {
	h := StatsStream(nil, statshub.New())

	req := httptest.NewRequest(http.MethodGet, "/stream?admin_key=test-admin-key", nil)
	rec := httptest.NewRecorder()

	h(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("valid ?admin_key= was rejected: status = %d, body: %s", rec.Code, rec.Body.String())
	}
}

// TestStatsStreamPingPong exercises a full handshake over a real listener
// (httptest.NewRecorder can't hijack, so the auth-gating tests above stop
// short of this): a correctly authenticated client completes the WS upgrade
// and the server answers an application-level ping with a pong (design doc
// §7). It uses a nil hub and never subscribes to a channel, so it never
// touches the (nil) database.
func TestStatsStreamPingPong(t *testing.T) {
	srv := httptest.NewServer(StatsStream(nil, statshub.New()))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Admin-Key": []string{"test-admin-key"}},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("Write ping: %v", err)
	}

	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	var env wsEnvelopeOut
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal response %s: %v", data, err)
	}
	if env.Type != "pong" {
		t.Fatalf("response type = %q, want %q (raw: %s)", env.Type, "pong", data)
	}
}

// TestStatsStreamDialRejectedWithoutAdminKey checks the client-visible side
// of the same auth gate: Dial itself fails (no upgrade completes) when the
// admin key is missing.
func TestStatsStreamDialRejectedWithoutAdminKey(t *testing.T) {
	srv := httptest.NewServer(StatsStream(nil, statshub.New()))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("Dial succeeded without an admin key")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("response status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}
