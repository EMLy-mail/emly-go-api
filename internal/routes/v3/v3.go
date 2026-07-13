package v3

import (
	emlyMiddleware "emly-api-go/internal/middleware"
	"net/http"

	"emly-api-go/internal/config"
	"emly-api-go/internal/handlers"
	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// NewRouter returns a chi.Router with all /v3 routes mounted.
//
// v3 is scoped to the multi-product updates API only (product-aware release
// management for both the EMLy app and the EMLy Updater) — it does not carry
// forward bug-reports/admin/auth, which are unaffected and remain on v1/v2.
func NewRouter(db *sqlx.DB, s3conn *storage.S3Connector) http.Handler {
	r := chi.NewRouter()

	rl := emlyMiddleware.NewRateLimiter(config.Load())

	r.Use(rl.Handler)

	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Server", "emly-api-go")
			w.Header().Set("X-Powered-By", "Rexouium in a suit")
			next.ServeHTTP(w, r)
		})
	})

	r.Get("/health", handlers.Health(db))

	registerUpdates(r, db, s3conn, config.Load().APIBaseURL, config.Load().UpdatesS3Prefix)

	return r
}
