package v2

import (
	apimw "emly-api-go/internal/middleware"
	"time"

	"emly-api-go/internal/handlers"
	"emly-api-go/internal/statshub"
	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/jmoiron/sqlx"
)

// registerUpdates mounts both update surfaces on the shared updates bucket:
// the EMLy client manifest/releases under s3Prefix, and the EMLy Updater's own
// self-update manifest/installer under updaterPrefix. hub may be nil; it is
// only used to publish updater_events for /v2/stats/stream (recordUpdaterEvent
// tolerates nil).
func registerUpdates(r chi.Router, db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix, updaterPrefix string, hub *statshub.Hub) {
	r.Route("/updates", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(httprate.LimitByIP(30, time.Minute))
			r.Get("/manifest", handlers.GetUpdateManifest(db, hub))
			r.Get("/releases/{version}/download", handlers.DownloadRelease(db, s3conn, s3Prefix, hub))
		})

		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/releases", handlers.ListReleases(db))
			r.Post("/releases", handlers.CreateRelease(db, s3conn, s3Prefix))
			r.Put("/releases/{version}", handlers.PutRelease(db))
			r.Patch("/releases/{version}", handlers.PatchRelease(db))
			r.Delete("/releases/{version}", handlers.DeleteRelease(db, s3conn, s3Prefix))
			r.Patch("/releases/{version}/channel", handlers.PatchReleaseChannels(db))
		})

		// The updater's self-update manifest is API-key authenticated, per the
		// self-update contract: a missing or wrong key is a 401 the client
		// logs and retries next cycle.
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/manifest/updater", handlers.GetUpdaterManifest(db, hub))
		})

		// The installer download stays public, like the EMLy release download:
		// the manifest's link may be served through a site mirror or CDN that
		// does not forward the API key.
		r.Group(func(r chi.Router) {
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/download/updater/{version}", handlers.DownloadUpdater(db, s3conn, updaterPrefix, hub))
		})

		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/updater/releases", handlers.ListUpdaterReleases(db))
			r.Post("/updater/releases", handlers.CreateUpdaterRelease(db, s3conn, updaterPrefix))
			r.Patch("/updater/releases/{version}", handlers.PatchUpdaterRelease(db))
			r.Delete("/updater/releases/{version}", handlers.DeleteUpdaterRelease(db, s3conn, updaterPrefix))
		})
	})
}
