package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
)

func Health(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")

		if err := db.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "db": "error"})
			return
		}

		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "db": "ok"})
	}
}
