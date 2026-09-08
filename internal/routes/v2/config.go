package v2

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/config"
	"emly-api-go/internal/handlers"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/jmoiron/sqlx"
)

// registerConfig mounts /v2/config: the public policy document (API-key
// protected like the updater manifest) and its admin routes (admin-key
// protected), per docs/superpowers/specs/2026-09-04-remote-config-api-design.md §5/§7.
func registerConfig(r chi.Router, db *sqlx.DB, cfg *config.Config) {
	r.Route("/config", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/", handlers.GetConfig(db))
		})

		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Post("/validate", handlers.ValidateConfig(db))
			r.Post("/preview", handlers.PreviewConfig(db))

			r.Get("/revisions", handlers.ListConfigRevisions(db))
			r.Post("/revisions", handlers.CreateConfigRevision(db, cfg))
			r.Get("/revisions/{revision}", handlers.GetConfigRevision(db))
			r.Delete("/revisions/{revision}", handlers.DeleteConfigRevision(db, cfg))
			r.Post("/revisions/{revision}/publish", handlers.PublishConfigRevision(db, cfg))

			r.Post("/rollback", handlers.RollbackConfig(db, cfg))
		})
	})
}
