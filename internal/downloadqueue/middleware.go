package downloadqueue

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"emly-api-go/internal/updaterclient"

	"github.com/go-chi/chi/v5"
)

// FullResponse is the body of the 429 a download gets when every slot is
// taken. RetryAfter repeats the Retry-After header for clients that only read
// the body.
type FullResponse struct {
	Error      string `json:"error"`
	Message    string `json:"message"`
	RetryAfter int    `json:"retry_after"`
	Capacity   int    `json:"capacity"`
	Active     int    `json:"active"`
}

// Middleware holds a slot for the whole of the wrapped download, or refuses
// it with 429 when the queue is enabled and full. product labels the slot on
// the admin routes; the version comes from the route's {version} parameter,
// so mount it with r.With on the download route itself.
//
// It must sit after the route's rate limiter: a request that limiter refuses
// never reaches here and never takes a slot.
//
// It also bounds the download's duration with the queue's DownloadTimeout.
// The two download routes are exempt from main.go's global 30s timeout
// (updates.IsInstallerDownload), which cut any installer on a link slower
// than ~330 KB/s, so this deadline is the only one they have.
func (q *Queue) Middleware(product string) func(http.Handler) http.Handler {
	return q.MiddlewareFunc(func(*http.Request) string { return product })
}

// MiddlewareFunc is Middleware for a route that serves more than one product
// (GET /v2/updates/{product}/releases/{version}/download): productOf names
// the slot's product per request, so mount it after whatever puts the product
// on the request.
func (q *Queue) MiddlewareFunc(productOf func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if q == nil {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithTimeout(r.Context(), DefaultDownloadTimeout)
				defer cancel()
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := updaterclient.IdentityFromRequest(r)
			version := chi.URLParam(r, "version")
			product := productOf(r)

			ctx, cancel := context.WithCancelCause(r.Context())
			defer cancel(nil)

			s, ok := q.acquire(SlotInfo{
				Product:  product,
				Version:  version,
				IP:       id.IP,
				Hostname: id.Hostname,
				HWID:     id.HWID,
			}, cancel)
			if !ok {
				st := q.Status()
				slog.InfoContext(r.Context(), "installer download refused: no free queue slot",
					"product", product, "version", version,
					"capacity", st.Capacity, "active", st.Active,
					"ip", id.IP, "host_name", id.Hostname, "hwid", id.HWID)

				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", strconv.Itoa(st.RetryAfterSeconds))
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(FullResponse{
					Error:      "download queue full",
					Message:    "No free download slots are available right now. Retry in " + strconv.Itoa(st.RetryAfterSeconds) + " seconds.",
					RetryAfter: st.RetryAfterSeconds,
					Capacity:   st.Capacity,
					Active:     st.Active,
				})
				return
			}
			// Evict cancels ctx with ErrEvicted; the deadline sits under it, so
			// context.Cause(dctx) still reports an eviction as one, and a
			// deadline as DeadlineExceeded.
			dctx, dcancel := context.WithTimeout(ctx, s.timeout)
			defer dcancel()

			cw := &countingWriter{ResponseWriter: w, slot: s}
			returned := false
			defer func() {
				reason := ReasonInternalError
				if returned {
					reason = cw.outcome(dctx)
				}
				q.Finish(s.info.ID, reason)
			}()

			next.ServeHTTP(cw, r.WithContext(dctx))
			returned = true
		})
	}
}

// countingWriter feeds the bytes a download has sent, and its Content-Length
// once the handler has set it, into the slot - what the admin routes turn
// into progress and average speed. Only the download's own goroutine
// touches it; the slot's counters are the shared, atomic part.
type countingWriter struct {
	http.ResponseWriter
	slot      *slot
	sawHeader bool
	status    int
	writeErr  error
}

// outcome is the Finish reason for the download once its handler returned:
// "" only when the installer went out whole. The truncation test mirrors
// streamInstaller's, so a transfer it logs as incomplete is never counted as
// completed here - even when the handler itself saw no error, as when the
// server timeout cancels the S3 read and io.Copy just stops short.
func (w *countingWriter) outcome(ctx context.Context) string {
	if w.status >= http.StatusBadRequest {
		return ReasonErrorResponse
	}
	sent, total := w.slot.sent.Load(), w.slot.total.Load()
	if w.writeErr != nil || (total > 0 && sent < total) {
		return FailureReason(ctx)
	}
	return ""
}

func (w *countingWriter) captureTotal() {
	if w.sawHeader {
		return
	}
	w.sawHeader = true
	if n, err := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64); err == nil {
		w.slot.setTotal(n)
	}
}

func (w *countingWriter) WriteHeader(code int) {
	w.captureTotal()
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.captureTotal()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.slot.addSent(n)
	if err != nil && w.writeErr == nil {
		w.writeErr = err
	}
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
