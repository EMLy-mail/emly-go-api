package updates

import (
	"net/http"
	"path"
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/downloadqueue"
	"emly-api-go/internal/productreg"
	"emly-api-go/internal/session"
	"emly-api-go/internal/statshub"
	"emly-api-go/internal/storage"
	"emly-api-go/internal/updaterclient"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// RegisterV2 mounts both update surfaces on the shared updates bucket: the
// per-product release manifest/downloads/management (EMLy and every other
// product in reg), and the EMLy Updater's own self-update manifest/installer
// under updaterPrefix. s3Prefix is S3_UPDATES_PREFIX, the base every
// product's folder is derived from (productreg.S3Prefix). hub may be nil; it
// is only used to publish updater_events for /v2/stats/stream
// (recordUpdaterEvent tolerates nil). queue caps how many installer downloads
// stream at once, across every product and the updater (nil means no cap; see
// internal/downloadqueue). reg may be nil, in which case only EMLy exists.
func RegisterV2(r chi.Router, db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix, updaterPrefix string, hub *statshub.Hub, queue *downloadqueue.Queue, reg *productreg.Registry) {
	r.Route("/updates", func(r chi.Router) {
		// The product-less routes predate products and always mean EMLy:
		// every EMLy client in the field polls /v2/updates/manifest and
		// downloads from the links it hands out, so they stay exactly where
		// they were. They are the same handlers as the /{product} routes
		// below, with the product fixed instead of read from the path.
		r.Group(func(r chi.Router) {
			r.Use(apimw.RouteLimitByIP(30, time.Minute))
			r.Use(reg.Fixed(productreg.EMLy))
			r.Get("/manifest", GetUpdateManifest(db, hub))
			r.With(queue.Middleware(productreg.EMLy)).
				Get("/releases/{version}/download", DownloadRelease(db, s3conn, s3Prefix, hub))
		})

		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))
			r.Use(reg.Fixed(productreg.EMLy))
			r.Use(session.LoadScope(db))
			r.Use(productreg.RequireAssigned)
			registerReleaseAdmin(r, db, s3conn, s3Prefix)
		})

		// Every product, EMLy included, under its own slug. Static segments
		// (manifest, releases, download, updater) win over {product} in chi,
		// which is why productreg reserves those names.
		r.Route("/{product}", func(r chi.Router) {
			// Public, like the EMLy routes above: a disabled product is a
			// 404 here, so releases can be staged before it goes live.
			r.Group(func(r chi.Router) {
				r.Use(apimw.RouteLimitByIP(30, time.Minute))
				r.Use(reg.Resolve(false))
				r.Get("/manifest", GetUpdateManifest(db, hub))
				r.With(queue.MiddlewareFunc(productSlug)).
					Get("/releases/{version}/download", DownloadRelease(db, s3conn, s3Prefix, hub))
			})

			// Admin key first, so an unauthenticated caller gets 401 for any
			// slug and cannot probe which products exist.
			r.Group(func(r chi.Router) {
				r.Use(apimw.AdminKeyAuth(db))
				r.Use(apimw.RouteLimitByIP(30, time.Minute))
				r.Use(reg.Resolve(true))
				r.Use(session.LoadScope(db))
				r.Use(productreg.RequireAssigned)
				registerReleaseAdmin(r, db, s3conn, s3Prefix)
			})
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

			r.With(queue.Middleware(updaterclient.ProductUpdater)).
				Get("/download/updater/{version}", DownloadUpdater(db, s3conn, updaterPrefix, hub))
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

// registerReleaseAdmin mounts release management for whichever product the
// group put on the context (productreg.Fixed or Resolve). The group must also
// mount session.LoadScope + productreg.RequireAssigned: a dashboard user only
// manages the releases of products assigned to them.
func registerReleaseAdmin(r chi.Router, db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string) {
	r.Get("/releases", ListReleases(db))
	r.Post("/releases", CreateRelease(db, s3conn, s3Prefix))
	r.Put("/releases/{version}", PutRelease(db))
	r.Patch("/releases/{version}", PatchRelease(db))
	r.Delete("/releases/{version}", DeleteRelease(db, s3conn, s3Prefix))
	r.Patch("/releases/{version}/channel", PatchReleaseChannels(db))
}

// productSlug labels a download-queue slot with the product Resolve put on
// the request.
func productSlug(r *http.Request) string { return productreg.FromContext(r.Context()).Slug }

// installerDownloadPatterns are the full paths of the installer download
// routes mounted above, as path.Match patterns ('*' never crosses a '/').
// TestInstallerDownloadPredicateMatchesRoutes in internal/routes/v2 walks the
// real router to keep them in step with RegisterV2.
var installerDownloadPatterns = []string{
	"/v2/updates/releases/*/download",
	"/v2/updates/*/releases/*/download",
	"/v2/updates/download/updater/*",
}

// IsInstallerDownload reports whether r targets one of the installer
// downloads. main.go keeps those out of the global 30s request timeout: that
// cut every installer on a link slower than ~330 KB/s, and their deadline
// now comes from the download queue instead (DOWNLOAD_QUEUE_TIMEOUT,
// changeable from the dashboard).
func IsInstallerDownload(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	for _, p := range installerDownloadPatterns {
		if ok, _ := path.Match(p, r.URL.Path); ok {
			return true
		}
	}
	return false
}
