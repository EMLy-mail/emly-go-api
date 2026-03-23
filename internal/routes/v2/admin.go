package v2

import (
	apimw "emly-api-go/internal/middleware"
	"time"

	"emly-api-go/internal/handlers"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/jmoiron/sqlx"
)

func registerAdmin(r chi.Router, db *sqlx.DB) {
	r.Route("/admin", func(r chi.Router) {
		r.Use(httprate.LimitByIP(30, time.Minute))

		// Auth — public, handles its own credential checks
		r.Route("/auth", func(r chi.Router) {
			r.Post("/login", handlers.LoginUser(db))
			r.Get("/validate", handlers.ValidateSession(db))
			r.Post("/logout", handlers.LogoutSession(db))
		})

		// User management — protected via Admin Key
		r.Route("/users", func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))

			r.Get("/", handlers.ListUsers(db))
			r.Post("/", handlers.CreateUser(db))
			r.Get("/{id}", handlers.GetUserByID(db))
			r.Patch("/{id}", handlers.UpdateUser(db))
			r.Post("/{id}/reset-password", handlers.ResetPassword(db))
			r.Delete("/{id}", handlers.DeleteUser(db))
		})
	})
}
