package clientlogs

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"emly-api-go/internal/clientproto"
)

func gzipB64(t *testing.T, b []byte) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func payload(t *testing.T, lf clientproto.LogFile) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(lf)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeLogFile(t *testing.T) {
	text := []byte("time=a level=INFO msg=one\ntime=b level=WARN msg=two\n")
	good := clientproto.LogFile{Name: "updater.log", SizeBytes: 100, Offset: 100 - int64(len(text)),
		IncludedBytes: int64(len(text)), Encoding: clientproto.LogEncodingGzipBase64,
		Content: gzipB64(t, text), CollectedAt: "2026-10-12T08:15:02Z"}

	d, err := decodeLogFile(payload(t, good))
	if err != nil || d.mismatch || d.file.Content != "" || len(d.gz) == 0 {
		t.Fatalf("good: d=%+v err=%v", d, err)
	}

	off := good
	off.IncludedBytes++
	if d, err := decodeLogFile(payload(t, off)); err != nil || !d.mismatch {
		t.Fatalf("length mismatch must be kept and flagged: d=%+v err=%v", d, err)
	}

	bad := map[string]func(*clientproto.LogFile){
		"encoding": func(l *clientproto.LogFile) { l.Encoding = "zstd+base64" },
		"base64":   func(l *clientproto.LogFile) { l.Content = "!!not base64!!" },
		"gzip":     func(l *clientproto.LogFile) { l.Content = base64.StdEncoding.EncodeToString(text) },
		"bomb": func(l *clientproto.LogFile) {
			l.Content = gzipB64(t, bytes.Repeat([]byte{'a'}, MaxDecompressedBytes+1))
		},
	}
	for name, mutate := range bad {
		lf := good
		mutate(&lf)
		if _, err := decodeLogFile(payload(t, lf)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := decodeLogFile(json.RawMessage(`[1,2]`)); err == nil {
		t.Error("non-object payload accepted")
	}
}

func TestParseClientTime(t *testing.T) {
	if got := parseClientTime("2026-10-12T10:15:02+02:00"); got == nil || got.Hour() != 8 || got.Location() != time.UTC {
		t.Fatalf("got %v", got)
	}
	if parseClientTime("") != nil || parseClientTime("yesterday") != nil {
		t.Fatal("garbage parsed")
	}
}

func TestAttachmentName(t *testing.T) {
	host := `PC-01"; evil=1`
	at := time.Date(2026, 10, 12, 8, 15, 2, 0, time.UTC)
	got := attachmentName(Record{ClientID: 7, Hostname: &host, FileName: "updater.log", CollectedAt: &at})
	if got != "PC-01___evil_1_20261012-081502_updater.log" {
		t.Fatalf("got %q", got)
	}
	got = attachmentName(Record{ClientID: 7, ReceivedAt: at, FileName: "../../x"})
	if got != "client-7_20261012-081502__.._x" || strings.ContainsAny(got, `/\"`) {
		t.Fatalf("got %q", got)
	}
}

func TestValidID(t *testing.T) {
	if !validID(clientproto.NewID()) {
		t.Fatal("a fresh ULID refused")
	}
	for _, id := range []string{"", "01J8ZQ6T3M6X9K2V7B4N1C5D8", "01J8ZQ6T3M6X9K2V7B4N1C5D8I", "../../../etc/passwd/aaaaaa"} {
		if validID(id) {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestPruneVictims(t *testing.T) {
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	cutoff := now.Add(-Retention)
	var rows []pruneRow
	// Client 1: MaxPerClient+2 recent logs, newest first -> the last 2 go.
	for i := range MaxPerClient + 2 {
		rows = append(rows, pruneRow{ID: "a" + string(rune('A'+i)), ClientID: 1, ReceivedAt: now.Add(-time.Duration(i) * time.Hour)})
	}
	// Client 2: one recent, one past retention.
	rows = append(rows,
		pruneRow{ID: "b1", ClientID: 2, ReceivedAt: now},
		pruneRow{ID: "b2", ClientID: 2, ReceivedAt: cutoff.Add(-time.Second)})
	// Client 3: deleted machine.
	rows = append(rows, pruneRow{ID: "c1", ClientID: 3, ReceivedAt: now, ClientGone: true})

	var ids []string
	for _, v := range pruneVictims(rows, cutoff) {
		ids = append(ids, v.ID)
	}
	want := []string{"a" + string(rune('A'+MaxPerClient)), "a" + string(rune('A'+MaxPerClient+1)), "b2", "c1"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("victims = %v, want %v", ids, want)
	}
}

func adminRouter(s *Store) http.Handler {
	r := chi.NewRouter()
	MountAdmin(r, s)
	return r
}

func TestRoutesRejectBadIDsBeforeTouchingStorage(t *testing.T) {
	r := adminRouter(New(nil, nil))
	cases := []struct{ method, path string }{
		{"GET", "/abc/logs"},
		{"GET", "/0/logs"},
		{"GET", "/logs/not-a-ulid"},
		{"GET", "/logs/not-a-ulid/content"},
		{"DELETE", "/logs/not-a-ulid"},
		{"GET", "/logs/" + clientproto.NewID() + "/content?format=zip"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s = %d, want 400", c.method, c.path, rec.Code)
		}
	}
}

func TestRoutesWithoutDatabaseAre503(t *testing.T) {
	r := adminRouter(New(nil, nil))
	id := clientproto.NewID()
	for _, c := range []struct{ method, path string }{
		{"GET", "/42/logs"},
		{"GET", "/logs/" + id},
		{"GET", "/logs/" + id + "/content"},
		{"DELETE", "/logs/" + id},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", c.method, c.path, rec.Code)
		}
	}
}
