package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
)

func Health(db *sqlx.DB) http.HandlerFunc {
	return HealthWithConfigMirror(db, nil)
}

// ConfigMirrorReporter is the health handler's view of a site mirror's
// remote-config replication state (internal/configmirror.State satisfies
// it). Kept as a narrow interface here rather than importing configmirror
// directly, so this package doesn't take on that dependency for every
// caller that just wants plain health.
type ConfigMirrorReporter interface {
	Snapshot() map[string]interface{}
}

// HealthWithConfigMirror is Health plus, when mirror is non-nil, a
// "config_upstream" field reporting this instance's remote-config
// replication state (API design doc §9) - only meaningful on a site mirror,
// so v1's /health and the root /health omit it by calling Health instead.
func HealthWithConfigMirror(db *sqlx.DB, mirror ConfigMirrorReporter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")

		body := map[string]interface{}{"status": "ok", "db": "ok"}
		status := http.StatusOK
		if err := db.PingContext(ctx); err != nil {
			status = http.StatusServiceUnavailable
			body["db"] = "error"
		}
		if mirror != nil {
			body["config_upstream"] = mirror.Snapshot()
		}

		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}
}
