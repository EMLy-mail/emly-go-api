package v1

import (
	"net/http"

	"emly-api-go/internal/handlers"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// NewRouter returns a chi.Router with all /v1 routes mounted.
// Add new API versions by creating an analogous package (e.g. v2) and
// mounting it alongside this one in internal/routes/routes.go.
func NewRouter(db *sqlx.DB) http.Handler {
	r := chi.NewRouter()

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
		registerBugReports(r, db)
	})

	return r
}
