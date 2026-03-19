package middleware

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/config"
)

func APIKeyAuth(_ *sqlx.DB) func(http.Handler) http.Handler {
	cfg := config.Load()

	if len(cfg.APIKey) == 0 {
		log.Panic("API key or admin key are empty")
		return nil
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
				log.Println("[API-KEY] Failed to authorize admin key for URL: " + r.URL.String())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
