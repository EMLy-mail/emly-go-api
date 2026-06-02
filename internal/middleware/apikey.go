package middleware

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/config"
)

func APIKeyAuth(_ *sqlx.DB) func(http.Handler) http.Handler {
	cfg := config.Load()

	if len(cfg.APIKey) == 0 {
		panic("API key is empty")
	}

	allowed := make(map[string]struct{}, 1)
	allowed[cfg.APIKey] = struct{}{}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-API-Key")
			if _, ok := allowed[key]; !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				slog.WarnContext(r.Context(), "api key auth failed", "url", r.URL.String())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
