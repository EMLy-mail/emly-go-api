package routes

import (
	"net/http"

	v1 "emly-api-go/internal/routes/v1"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// RegisterAll mounts every versioned API onto the root router.
// To add a new API version, create internal/routes/v2 and add:
//
//	r.Mount("/v2", v2.NewRouter(db))
func RegisterAll(r chi.Router, db *sqlx.DB) {
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("emly-api-go"))
		if err != nil {
			return
		}
	})

	r.Mount("/v1", v1.NewRouter(db))
}
