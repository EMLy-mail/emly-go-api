package routes

import (
	"net/http"

	v1 "emly-api-go/internal/routes/v1"
	v2 "emly-api-go/internal/routes/v2"
	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// RegisterAll mounts every versioned API onto the root router.
func RegisterAll(r chi.Router, db *sqlx.DB, s3conn *storage.S3Connector) {
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("emly-api-go"))
		if err != nil {
			return
		}
	})

	r.Mount("/v1", v1.NewRouter(db, s3conn))
	r.Mount("/v2", v2.NewRouter(db, s3conn))
}
