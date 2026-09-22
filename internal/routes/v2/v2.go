package v2

import (
	"emly-api-go/internal/admin"
	"emly-api-go/internal/bans"
	"emly-api-go/internal/bugreports"
	"emly-api-go/internal/clientws"
	"emly-api-go/internal/config"
	"emly-api-go/internal/configapi"
	"emly-api-go/internal/health"
	emlyMiddleware "emly-api-go/internal/middleware"
	"emly-api-go/internal/presencehub"
	"emly-api-go/internal/stats"
	"emly-api-go/internal/statshub"
	"emly-api-go/internal/storage"
	"emly-api-go/internal/updates"
	"net/http"

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
// pushed updates; see statshub and stats.StatsStream). presence backs
// GET /v2/client/ws and the "online" field on GET /v2/stats/clients /
// stats:clients (nil is fine; see internal/presencehub). bans is the live
// block-list snapshot the admin routes refresh after a write; nil is fine in
// tests, where the write lands in the table and nothing needs to enforce it.
func NewRouter(db *sqlx.DB, apiFileS3conn, updatesS3conn *storage.S3Connector, configMirror health.ConfigMirrorReporter, hub *statshub.Hub, presence *presencehub.Hub, reloader bans.BanReloader) http.Handler {
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

	r.Get("/health", health.HealthWithConfigMirror(db, configMirror))

	updates.RegisterV2(r, db, updatesS3conn, config.Load().UpdatesS3Prefix, config.Load().UpdaterS3Prefix, hub)
	stats.RegisterV2(r, db, config.Load(), hub, presence)
	configapi.RegisterV2(r, db, config.Load())
	bans.RegisterV2(r, db, reloader)
	clientws.RegisterV2(r, db, presence)

	r.Route("/api", func(r chi.Router) {
		admin.RegisterV2(r, db)
		bugreports.RegisterV2(r, db, config.Load().Database, apiFileS3conn)
	})

	return r
}
