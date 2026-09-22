package stats

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/config"
	"emly-api-go/internal/presencehub"
	"emly-api-go/internal/statshub"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// RegisterV2 mounts both the REST stats/* endpoints (admin-key gated,
// like the rest of this group) and their real-time counterpart,
// /stats/stream. hub may be nil (tests, or a build with the WS stream
// unused); StatsStream and recordUpdaterEvent both tolerate that.
// presence backs the "online" field on GET /stats/clients and the
// stats:clients WS channel; nil is fine (see internal/presencehub). cfg
// carries StatsCacheTTL, which bounds how stale the polled /summary and
// /events may be - see GetStatsSummary and GetStatsEvents.
func RegisterV2(r chi.Router, db *sqlx.DB, cfg *config.Config, hub *statshub.Hub, presence *presencehub.Hub) {
	r.Route("/stats", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/summary", GetStatsSummary(db, cfg))
			r.Get("/clients", ListStatsClients(db, presence))
			r.Get("/clients/{id}", GetStatsClientDetail(db, presence))
			r.Get("/events", GetStatsEvents(db, cfg))
		})

		// /stream does its own X-Admin-Key check (with a query-string
		// fallback for a proxy that strips custom headers on the Upgrade
		// request) before completing the WS upgrade, rather than going
		// through apimw.AdminKeyAuth - see StatsStream and the
		// design doc §4.
		r.Group(func(r chi.Router) {
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/stream", StatsStream(db, hub, presence))
		})
	})
}
