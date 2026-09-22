package bans

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// RegisterV2 mounts the permanent block list's admin surface. Admin-key
// gated like stats and the config writes: this decides who can reach the API
// at all, so it is not something a session token alone should change.
//
// reloader may be nil in tests; the handlers then skip the snapshot refresh and
// the write still lands in the table.
func RegisterV2(r chi.Router, db *sqlx.DB, reloader BanReloader) {
	r.Route("/bans", func(r chi.Router) {
		r.Use(apimw.AdminKeyAuth(db))
		r.Use(apimw.RouteLimitByIP(30, time.Minute))

		r.Get("/", ListBans(db))
		r.Post("/", CreateBan(db, reloader))
		r.Delete("/{id}", DeleteBan(db, reloader))
	})
}
