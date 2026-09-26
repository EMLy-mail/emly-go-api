package admin

import (
	"log/slog"
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/config"
	"emly-api-go/internal/oidc"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func RegisterV2(r chi.Router, db *sqlx.DB) {
	oidcCfg := config.Load().OIDC
	verifier := oidc.NewVerifier(oidcCfg)
	if verifier.Enabled() {
		slog.Info("sso: enabled", "issuer", oidcCfg.Issuer, "client_id", oidcCfg.ClientID)
	} else {
		slog.Info("sso: disabled (set OIDC_ISSUER and OIDC_CLIENT_ID to enable)")
	}

	r.Route("/admin", func(r chi.Router) {
		r.Use(apimw.RouteLimitByIP(30, time.Minute))

		// Auth — public, handles its own credential checks
		r.Route("/auth", func(r chi.Router) {
			r.Post("/login", LoginUser(db))
			r.Get("/validate", ValidateSession(db))
			r.Post("/logout", LogoutSession(db))

			// SSO exchange: called by the dashboard server with the ID token it
			// got from the identity provider, so it is admin-key gated.
			r.With(apimw.AdminKeyAuth(db)).Post("/oidc", LoginOIDC(db, verifier, oidcCfg))
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
