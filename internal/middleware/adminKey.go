package middleware

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/config"
)

func AdminKeyAuth(_ *sqlx.DB) func(http.Handler) http.Handler {
	cfg := config.Load()

	if len(cfg.AdminKey) == 0 {
		log.Panic("API key or admin key are empty")
		return nil
	}

	allowed := make(map[string]struct{}, 1)
	allowed[cfg.AdminKey] = struct{}{}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-Admin-Key")
			if _, ok := allowed[key]; !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized admin key"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
