package v2

import (
	emlyMiddleware "emly-api-go/internal/middleware"
	"net/http"

	"emly-api-go/internal/config"
	"emly-api-go/internal/handlers"
	"emly-api-go/internal/statshub"
	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// NewRouter returns a chi.Router with all /v2 routes mounted. apiFileS3conn
// backs bug-report file attachments and updatesS3conn backs update-release
// installers; the two are independent connectors and may live on different
// S3-compatible providers. configMirror is non-nil only on a site mirror
// (CONFIG_UPSTREAM_URL set) and adds its replication state to /v2/health;
// pass nil on the cloud/primary instance and in tests. hub feeds
// /v2/stats/stream (nil is fine - the route degrades to snapshots-only, no
// pushed updates; see statshub and handlers.StatsStream). bans is the live
// block-list snapshot the admin routes refresh after a write; nil is fine in
// tests, where the write lands in the table and nothing needs to enforce it.
func NewRouter(db *sqlx.DB, apiFileS3conn, updatesS3conn *storage.S3Connector, configMirror handlers.ConfigMirrorReporter, hub *statshub.Hub, bans handlers.BanReloader) http.Handler {
	r := chi.NewRouter()

	rl := emlyMiddleware.NewRateLimiter(config.Load())

	r.Use(rl.Handler)

	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Server", "emly-api-go")
			w.Header().Set("X-Powered-By", "Rexouium in a suit")
			next.ServeHTTP(w, r)
		})
	})

	r.Get("/health", handlers.HealthWithConfigMirror(db, configMirror))

	registerUpdates(r, db, updatesS3conn, config.Load().UpdatesS3Prefix, config.Load().UpdaterS3Prefix, hub)
	registerStats(r, db, config.Load(), hub)
	registerConfig(r, db, config.Load())
	registerBans(r, db, bans)

	r.Route("/api", func(r chi.Router) {
		registerAdmin(r, db)
		registerBugReports(r, db, config.Load().Database, apiFileS3conn)
	})

	return r
}
