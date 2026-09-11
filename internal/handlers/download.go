package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	chiMiddleware "github.com/go-chi/chi/v5/middleware"
)

// streamInstaller copies an installer body to the client and reports, at warn
// level, every way that copy can end short.
//
// By the time io.Copy runs, the 200 and the Content-Length header are already
// on the wire, so nothing that goes wrong from here on can be turned into an
// HTTP error - this log line is the only record that the client was left with
// a truncated file. The case worth watching is "server timeout": the global
// chiMiddleware.Timeout(30s) in main.go cancels r.Context() mid-transfer, the
// S3 read fails, and chi's deferred w.WriteHeader(504) is discarded by
// net/http with "superfluous response.WriteHeader call" because the status
// line went out long ago. The access log then records a 504 for a request the
// client saw as a 200 with a short body. A slow link is enough to trigger it:
// an 8 MB partial transfer over 30s is ~270 KB/s.
func streamInstaller(w http.ResponseWriter, r *http.Request, src io.Reader, product, version, filename string, size int64) {
	started := time.Now()
	written, err := io.Copy(w, src)
	elapsed := time.Since(started)

	truncated := size > 0 && written < size
	if err == nil && !truncated {
		return
	}

	reason := "copy failed"
	switch {
	case errors.Is(r.Context().Err(), context.DeadlineExceeded):
		reason = "server timeout"
	case errors.Is(r.Context().Err(), context.Canceled):
		reason = "client disconnected"
	}

	args := []any{
		"request_id", chiMiddleware.GetReqID(r.Context()),
		"reason", reason,
		"product", product,
		"version", version,
		"filename", filename,
		"bytes_sent", written,
		"duration", elapsed.String(),
	}
	if size > 0 {
		args = append(args, "bytes_expected", size)
	}
	if secs := elapsed.Seconds(); secs > 0 {
		args = append(args, "avg_kb_s", int64(float64(written)/secs/1024))
	}
	if err != nil {
		args = append(args, "err", err)
	}

	id := clientIdentityFromRequest(r)
	if id.Hostname != "" {
		args = append(args, "host_name", id.Hostname)
	}
	if id.HWID != "" {
		args = append(args, "hwid", id.HWID)
	}
	if id.IP != "" {
		args = append(args, "ip", id.IP)
	}

	// context.WithoutCancel: r.Context() is usually already dead here, and a
	// canceled context suppresses the OTel log export.
	slog.WarnContext(context.WithoutCancel(r.Context()), "installer download did not complete", args...)
}
