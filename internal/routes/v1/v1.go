package v1

import (
	emlyMiddleware "emly-api-go/internal/middleware"
	"net/http"

	"emly-api-go/internal/config"
	"emly-api-go/internal/handlers"
	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// NewRouter returns a chi.Router with all /v1 routes mounted.
func NewRouter(db *sqlx.DB, s3conn *storage.S3Connector) http.Handler {
	r := chi.NewRouter()

	rl := emlyMiddleware.NewRateLimiter(config.Load())

	r.Use(rl.Handler)

	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Server", "emly-api-go")
			w.Header().Set("X-Powered-By", "Pure Protogen sillyness :3")
			next.ServeHTTP(w, r)
		})
	})

	r.Get("/health", handlers.Health(db))

	r.Route("/api", func(r chi.Router) {
		registerAdmin(r, db)
		registerBugReports(r, db, config.Load().Database, s3conn)
	})

	return r
}
