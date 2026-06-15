package routes

import (
	"emly-api-go/internal/config"
	"emly-api-go/internal/handlers"
	apimw "emly-api-go/internal/middleware"
	"net/http"
	"time"

	v1 "emly-api-go/internal/routes/v1"
	v2 "emly-api-go/internal/routes/v2"
	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/jmoiron/sqlx"
)

// RegisterAll mounts every versioned API onto the root router.
func RegisterAll(r chi.Router, db *sqlx.DB, s3conn *storage.S3Connector) {
	dbName := config.Load().Database

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("emly-api-go"))
		if err != nil {
			return
		}
	})

	r.Mount("/v1", v1.NewRouter(db, s3conn))
	r.Mount("/v2", v2.NewRouter(db, s3conn))
	// Redirect /health to /v1/health
	r.Get("/health", handlers.Health(db))

	// Legacy compatibility: expose bug-report creation also under /api/bug-reports.
	r.Post("/api/bug-reports", registerBugReports(r, db, dbName, s3conn))

}

func registerBugReports(_ chi.Router, db *sqlx.DB, dbName string, s3conn *storage.S3Connector) http.HandlerFunc {
	h := handlers.CreateBugReport(db, dbName, s3conn)
	h = apimw.APIKeyAuth(db)(h).ServeHTTP
	h = httprate.LimitByIP(30, time.Minute)(h).ServeHTTP
	return h
}
