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
func (q *Queue) Middleware(product string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if q == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := updaterclient.IdentityFromRequest(r)
			version := chi.URLParam(r, "version")

			ctx, cancel := context.WithCancelCause(r.Context())
			defer cancel(nil)

			slotID, ok := q.TryAcquire(SlotInfo{
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
			defer q.Release(slotID)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
