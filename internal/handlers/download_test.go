package handlers

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captureHandler collects the records written to slog so a test can assert on
// the level and the attributes of one log line.
type captureHandler struct {
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func captureLogs(t *testing.T) *captureHandler {
	t.Helper()
	logs := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

func attrs(r slog.Record) map[string]any {
	m := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	return m
}

// shortReader yields n bytes and then fails, the way an S3 body fails once the
// request context it was opened with is cancelled mid-transfer.
type shortReader struct {
	left int
	err  error
}

func (s *shortReader) Read(p []byte) (int, error) {
	if s.left <= 0 {
		return 0, s.err
	}
	n := len(p)
	if n > s.left {
		n = s.left
	}
	s.left -= n
	return n, nil
}

func TestStreamInstallerQuietOnCompleteCopy(t *testing.T) {
	logs := captureLogs(t)

	body := strings.Repeat("x", 2048)
	r := httptest.NewRequest("GET", "/v2/updates/releases/2.2.2/download", nil)
	w := httptest.NewRecorder()

	streamInstaller(w, r, strings.NewReader(body), productEMLy, "2.2.2", "EMLy_2.2.2.exe", int64(len(body)))

	if len(logs.records) != 0 {
		t.Fatalf("expected no log records for a complete copy, got %d", len(logs.records))
	}
	if got := w.Body.Len(); got != len(body) {
		t.Fatalf("wrote %d bytes, want %d", got, len(body))
	}
}

func TestStreamInstallerWarnsOnServerTimeout(t *testing.T) {
	logs := captureLogs(t)

	// The global chiMiddleware.Timeout deadline expiring mid-download: the
	// request context is DeadlineExceeded and the S3 read fails short.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	r := httptest.NewRequest("GET", "/v2/updates/releases/2.2.2/download", nil).WithContext(ctx)
	r.Header.Set("X-EMLy-Hostname", "PCRM034")
	r.Header.Set("X-EMLy-HWID", "76D82CDC-378B-5C3C-FDF8-C2283F93BA11")
	w := httptest.NewRecorder()

	streamInstaller(w, r, &shortReader{left: 4096, err: context.DeadlineExceeded},
		productEMLy, "2.2.2", "EMLy_2.2.2.exe", 50_000_000)

	if len(logs.records) != 1 {
		t.Fatalf("expected exactly one log record, got %d", len(logs.records))
	}
	rec := logs.records[0]
	if rec.Level != slog.LevelWarn {
		t.Fatalf("level = %v, want WARN", rec.Level)
	}
	a := attrs(rec)
	if a["reason"] != "server timeout" {
		t.Fatalf("reason = %v, want \"server timeout\"", a["reason"])
	}
	if a["bytes_sent"] != int64(4096) {
		t.Fatalf("bytes_sent = %v, want 4096", a["bytes_sent"])
	}
	if a["bytes_expected"] != int64(50_000_000) {
		t.Fatalf("bytes_expected = %v, want 50000000", a["bytes_expected"])
	}
	if a["version"] != "2.2.2" || a["product"] != productEMLy {
		t.Fatalf("product/version = %v/%v", a["product"], a["version"])
	}
	if a["host_name"] != "PCRM034" {
		t.Fatalf("host_name = %v", a["host_name"])
	}
}

func TestStreamInstallerWarnsOnClientDisconnect(t *testing.T) {
	logs := captureLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := httptest.NewRequest("GET", "/v2/updates/download/updater/1.6.2", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	streamInstaller(w, r, &shortReader{left: 10, err: io.ErrUnexpectedEOF},
		productUpdater, "1.6.2", "EMLyUpdater_Installer_1.6.2.exe", 1000)

	if len(logs.records) != 1 {
		t.Fatalf("expected exactly one log record, got %d", len(logs.records))
	}
	if got := attrs(logs.records[0])["reason"]; got != "client disconnected" {
		t.Fatalf("reason = %v, want \"client disconnected\"", got)
	}
}

// A short body with no context error at all is still a truncated download and
// must not pass silently: io.Copy returns nil when the source simply ends.
func TestStreamInstallerWarnsOnShortBodyWithoutError(t *testing.T) {
	logs := captureLogs(t)

	r := httptest.NewRequest("GET", "/v2/updates/releases/2.2.2/download", nil)
	w := httptest.NewRecorder()

	streamInstaller(w, r, strings.NewReader("partial"), productEMLy, "2.2.2", "EMLy_2.2.2.exe", 900)

	if len(logs.records) != 1 {
		t.Fatalf("expected exactly one log record, got %d", len(logs.records))
	}
	if got := attrs(logs.records[0])["reason"]; got != "copy failed" {
		t.Fatalf("reason = %v, want \"copy failed\"", got)
	}
}
