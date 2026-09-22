package v1

import (
	apimw "emly-api-go/internal/middleware"
	"time"

	"emly-api-go/internal/admin"
	"emly-api-go/internal/bugreports"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func registerAdmin(r chi.Router, db *sqlx.DB, dbName string) {
	r.Route("/admin", func(r chi.Router) {

		// Auth — public, handles its own credential checks.
		// Only /login is rate-limited: it is the only endpoint vulnerable to
		// brute-force. /validate and /logout require a 256-bit session token
		// and are called frequently by authenticated clients, so no limit is
		// applied there.
		r.Route("/auth", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(apimw.RouteLimitByIP(30, time.Minute))
				r.Post("/login", admin.LoginUser(db))
			})
			r.Get("/validate", admin.ValidateSession(db))
			r.Post("/logout", admin.LogoutSession(db))
		})

		// User management — protected via Admin Key
		r.Route("/users", func(r chi.Router) {
			r.Use(apimw.RouteLimitByIP(30, time.Minute))
			r.Use(apimw.AdminKeyAuth(db))

			r.Get("/", admin.ListUsers(db))
			r.Post("/", admin.CreateUser(db))
			r.Get("/{id}", admin.GetUserByID(db))
			r.Patch("/{id}", admin.UpdateUser(db))
			r.Post("/{id}/reset-password", admin.ResetPassword(db))
			r.Delete("/{id}", admin.DeleteUser(db))
		})

		// Backward-compatible alias for admin-prefixed bug report delete path.
		r.Route("/bug-reports", func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Delete("/{id}", bugreports.DeleteBugReportByID(db, dbName))
		})
	})
}
