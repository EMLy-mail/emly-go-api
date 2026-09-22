package admin

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func RegisterV2(r chi.Router, db *sqlx.DB) {
	r.Route("/admin", func(r chi.Router) {
		r.Use(apimw.RouteLimitByIP(30, time.Minute))

		// Auth — public, handles its own credential checks
		r.Route("/auth", func(r chi.Router) {
			r.Post("/login", LoginUser(db))
			r.Get("/validate", ValidateSession(db))
			r.Post("/logout", LogoutSession(db))
		})

		// User management — protected via Admin Key
		r.Route("/users", func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))

			r.Get("/", ListUsers(db))
			r.Post("/", CreateUser(db))
			r.Get("/{id}", GetUserByID(db))
			r.Patch("/{id}", UpdateUser(db))
			r.Post("/{id}/reset-password", ResetPassword(db))
			r.Delete("/{id}", DeleteUser(db))
		})
	})
}
