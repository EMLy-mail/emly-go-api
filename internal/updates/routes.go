package updates

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/statshub"
	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// RegisterV2 mounts both update surfaces on the shared updates bucket:
// the EMLy client manifest/releases under s3Prefix, and the EMLy Updater's own
// self-update manifest/installer under updaterPrefix. hub may be nil; it is
// only used to publish updater_events for /v2/stats/stream (recordUpdaterEvent
// tolerates nil).
func RegisterV2(r chi.Router, db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix, updaterPrefix string, hub *statshub.Hub) {
	r.Route("/updates", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(apimw.RouteLimitByIP(30, time.Minute))
			r.Get("/manifest", GetUpdateManifest(db, hub))
			r.Get("/releases/{version}/download", DownloadRelease(db, s3conn, s3Prefix, hub))
		})

		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/releases", ListReleases(db))
			r.Post("/releases", CreateRelease(db, s3conn, s3Prefix))
			r.Put("/releases/{version}", PutRelease(db))
			r.Patch("/releases/{version}", PatchRelease(db))
			r.Delete("/releases/{version}", DeleteRelease(db, s3conn, s3Prefix))
			r.Patch("/releases/{version}/channel", PatchReleaseChannels(db))
		})

		// The updater's self-update manifest is API-key authenticated, per the
		// self-update contract: a missing or wrong key is a 401 the client
		// logs and retries next cycle.
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/manifest/updater", GetUpdaterManifest(db, hub))
		})

		// The installer download stays public, like the EMLy release download:
		// the manifest's link may be served through a site mirror or CDN that
		// does not forward the API key.
		r.Group(func(r chi.Router) {
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/download/updater/{version}", DownloadUpdater(db, s3conn, updaterPrefix, hub))
		})

		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/updater/releases", ListUpdaterReleases(db))
			r.Post("/updater/releases", CreateUpdaterRelease(db, s3conn, updaterPrefix))
			r.Patch("/updater/releases/{version}", PatchUpdaterRelease(db))
			r.Delete("/updater/releases/{version}", DeleteUpdaterRelease(db, s3conn, updaterPrefix))
		})
	})
}
