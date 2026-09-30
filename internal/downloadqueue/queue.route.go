package downloadqueue

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"emly-api-go/internal/dbvalue"
	"emly-api-go/internal/response"
	"emly-api-go/internal/session"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// GetQueue handles GET /v2/download-queue: settings, counters and every
// download currently holding a slot.
func GetQueue(q *Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q == nil {
			response.Error(w, http.StatusServiceUnavailable, "download queue is not configured")
			return
		}
		response.OK(w, q.Status())
	}
}

// PatchQueue handles PATCH /v2/download-queue: switch the limit on/off, grow
// or shrink the capacity, change the Retry-After and the download timeout.
// In memory only - a restart goes back to the .env values.
func PatchQueue(db *sqlx.DB, q *Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q == nil {
			response.Error(w, http.StatusServiceUnavailable, "download queue is not configured")
			return
		}
		var p Patch
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if p.Empty() {
			response.Error(w, http.StatusBadRequest, "nothing to update: send enabled, capacity, retry_after_seconds or download_timeout_seconds")
			return
		}
		st, err := q.Update(p)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, ErrInvalidPatch) {
				status = http.StatusBadRequest
			}
			response.Error(w, status, err.Error())
			return
		}
		slog.InfoContext(r.Context(), "download queue settings changed",
			"by", actor(r, db),
			"enabled", st.Enabled, "capacity", st.Capacity,
			"retry_after_seconds", st.RetryAfterSeconds,
			"download_timeout_seconds", st.DownloadTimeoutSeconds, "active", st.Active)
		response.OK(w, st)
	}
}

// ResetQueue handles POST /v2/download-queue/reset: back to the .env values.
func ResetQueue(db *sqlx.DB, q *Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q == nil {
			response.Error(w, http.StatusServiceUnavailable, "download queue is not configured")
			return
		}
		st := q.Reset()
		slog.InfoContext(r.Context(), "download queue settings reset to defaults",
			"by", actor(r, db),
			"enabled", st.Enabled, "capacity", st.Capacity,
			"retry_after_seconds", st.RetryAfterSeconds,
			"download_timeout_seconds", st.DownloadTimeoutSeconds)
		response.OK(w, st)
	}
}

// EvictSlot handles DELETE /v2/download-queue/slots/{id}: frees the slot and
// cancels the transfer, which the client sees as a truncated download.
func EvictSlot(db *sqlx.DB, q *Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q == nil {
			response.Error(w, http.StatusServiceUnavailable, "download queue is not configured")
			return
		}
		id, err := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
		if err != nil || id == 0 {
			response.Error(w, http.StatusBadRequest, "invalid slot id")
			return
		}
		if !q.Evict(id) {
			response.Error(w, http.StatusNotFound, "slot not found")
			return
		}
		slog.InfoContext(r.Context(), "download evicted from queue slot",
			"by", actor(r, db), "slot_id", id)
		response.OK(w, map[string]any{"evicted": 1})
	}
}

// EvictAllSlots handles DELETE /v2/download-queue/slots: frees every slot.
func EvictAllSlots(db *sqlx.DB, q *Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q == nil {
			response.Error(w, http.StatusServiceUnavailable, "download queue is not configured")
			return
		}
		n := q.EvictAll()
		slog.InfoContext(r.Context(), "all downloads evicted from queue slots",
			"by", actor(r, db), "evicted", n)
		response.OK(w, map[string]any{"evicted": n})
	}
}

// actor is who made an admin change, for the log line only: the queue itself
// is in memory and writes nothing to the database.
func actor(r *http.Request, db *sqlx.DB) string {
	if db == nil {
		return ""
	}
	return dbvalue.Deref(session.Username(r, db))
}
