package configapi

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/config"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// RegisterV2 mounts /v2/config: the public policy document (API-key
// protected like the updater manifest) and its admin routes (admin-key
// protected), per docs/superpowers/specs/2026-09-04-remote-config-api-design.md §5/§7.
func RegisterV2(r chi.Router, db *sqlx.DB, cfg *config.Config) {
	r.Route("/config", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/", GetConfig(db))
		})

		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Post("/validate", ValidateConfig(db))
			r.Post("/preview", PreviewConfig(db))

			r.Get("/revisions", ListConfigRevisions(db))
			r.Post("/revisions", CreateConfigRevision(db, cfg))
			r.Get("/revisions/{revision}", GetConfigRevision(db))
			r.Delete("/revisions/{revision}", DeleteConfigRevision(db, cfg))
			r.Post("/revisions/{revision}/publish", PublishConfigRevision(db, cfg))

			r.Post("/rollback", RollbackConfig(db, cfg))
		})
	})
}
