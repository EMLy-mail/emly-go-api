package clientws

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
		// M1: a huge ttl_seconds overflows time.Duration(ttl_seconds)*time.Second
		// (int64) before any post-conversion range check would catch it; the
		// fix bounds-checks the raw seconds value first.
		{"/42/commands", `{"name":"machine.info","ttl_seconds":9000000000000000}`, 400},
		{"/42/commands", `{"name":"machine.format_disk"}`, 422},
		{"/42/commands", `{"name":"machine.info"}`, 409},
		{"/1/commands", `{"name":"machine.info"}`, 422},
		// M2: issued_by is capped at 64 chars and must be printable.
		{"/42/commands", `{"name":"machine.info","issued_by":"` + strings.Repeat("a", 65) + `"}`, 400},
		{"/42/commands", `{"name":"machine.info","issued_by":"admin\u0000f.fois"}`, 400},
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
