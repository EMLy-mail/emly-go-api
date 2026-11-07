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
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/clienthub"
	"emly-api-go/internal/clientproto"
)

func adminRouter(hub *clienthub.Hub) http.Handler {
	r := chi.NewRouter()
	mountAdmin(r, nil, hub)
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

// stubHistory replaces the history query, recording what it was asked and
// answering with rows.
func stubHistory(t *testing.T, rows []HistoryEvent) *historyQuery {
	t.Helper()
	got := &historyQuery{}
	prev := listWSEventsFn
	listWSEventsFn = func(_ context.Context, _ *sqlx.DB, q historyQuery) ([]HistoryEvent, error) {
		*got = q
		if len(rows) > q.Limit {
			return rows[:q.Limit], nil
		}
		return rows, nil
	}
	t.Cleanup(func() { listWSEventsFn = prev })
	return got
}

func TestEventHistoryWithoutDatabaseIs503(t *testing.T) {
	rec := do(t, adminRouter(nil), "GET", "/42/events/history", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestEventHistoryParamsAndPaging(t *testing.T) {
	rows := []HistoryEvent{
		{ID: 9, EventID: clientproto.NewID(), ClientID: 42, Name: "service.stopped", Payload: json.RawMessage(`{"reason":"shutdown"}`)},
		{ID: 7, EventID: clientproto.NewID(), ClientID: 42, Name: "service.stopped"},
	}
	got := stubHistory(t, rows)

	rec := do(t, adminRouter(nil), "GET",
		"/42/events/history?name=service.stopped&from=2026-10-01T00:00:00Z&to=2026-10-02T00:00:00%2B02:00&before=10&limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	want := historyQuery{ClientID: 42, Name: "service.stopped", Before: 10, Limit: 2,
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 1, 22, 0, 0, 0, time.UTC)}
	if !got.From.Equal(want.From) || !got.To.Equal(want.To) || got.ClientID != want.ClientID ||
		got.Name != want.Name || got.Before != want.Before || got.Limit != want.Limit {
		t.Fatalf("query = %+v, want %+v", *got, want)
	}
	var body struct {
		Events []struct {
			ID      int64           `json:"id"`
			Payload json.RawMessage `json:"payload"`
		} `json:"events"`
		NextBefore *int64 `json:"next_before"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 2 || string(body.Events[0].Payload) != `{"reason":"shutdown"}` || body.Events[1].Payload != nil {
		t.Fatalf("events = %s", rec.Body)
	}
	if body.NextBefore == nil || *body.NextBefore != 7 {
		t.Fatalf("next_before = %v, want 7 on a full page", body.NextBefore)
	}

	// A short page is the last: no cursor. Defaults apply without params.
	rec = do(t, adminRouter(nil), "GET", "/42/events/history", "")
	if strings.Contains(rec.Body.String(), "next_before") {
		t.Fatalf("short page carries a cursor: %s", rec.Body)
	}
	if got.Limit != defaultHistoryLimit || got.Name != "" || got.Before != 0 || !got.From.IsZero() {
		t.Fatalf("defaults = %+v", *got)
	}
}

func TestEventHistoryRejectsBadParams(t *testing.T) {
	stubHistory(t, nil)
	for _, path := range []string{
		"/0/events/history",
		"/abc/events/history",
		"/42/events/history?name=machine.info",
		"/42/events/history?name=made.up",
		"/42/events/history?from=yesterday",
		"/42/events/history?from=2026-10-02T00:00:00Z&to=2026-10-01T00:00:00Z",
		"/42/events/history?before=-1",
		"/42/events/history?limit=0",
		"/42/events/history?limit=501",
	} {
		if rec := do(t, adminRouter(nil), "GET", path, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, rec.Code)
		}
	}
}
