package v2

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/handlers"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/jmoiron/sqlx"
)

// registerBans mounts the permanent block list's admin surface. Admin-key
// gated like stats and the config writes: this decides who can reach the API
// at all, so it is not something a session token alone should change.
//
// bans may be nil in tests; the handlers then skip the snapshot refresh and
// the write still lands in the table.
func registerBans(r chi.Router, db *sqlx.DB, bans handlers.BanReloader) {
	r.Route("/bans", func(r chi.Router) {
		r.Use(apimw.AdminKeyAuth(db))
		r.Use(httprate.LimitByIP(30, time.Minute))

		r.Get("/", handlers.ListBans(db))
		r.Post("/", handlers.CreateBan(db, bans))
		r.Delete("/{id}", handlers.DeleteBan(db, bans))
	})
}
