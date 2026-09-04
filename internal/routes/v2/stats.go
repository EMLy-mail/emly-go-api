package v2

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/handlers"
	"emly-api-go/internal/statshub"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/jmoiron/sqlx"
)

// registerStats mounts both the REST stats/* endpoints (admin-key gated,
// like the rest of this group) and their real-time counterpart,
// /stats/stream. hub may be nil (tests, or a build with the WS stream
// unused); handlers.StatsStream and recordUpdaterEvent both tolerate that.
func registerStats(r chi.Router, db *sqlx.DB, hub *statshub.Hub) {
	r.Route("/stats", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/summary", handlers.GetStatsSummary(db))
			r.Get("/clients", handlers.ListStatsClients(db))
			r.Get("/clients/{id}", handlers.GetStatsClientDetail(db))
			r.Get("/events", handlers.GetStatsEvents(db))
		})

		// /stream does its own X-Admin-Key check (with a query-string
		// fallback for a proxy that strips custom headers on the Upgrade
		// request) before completing the WS upgrade, rather than going
		// through apimw.AdminKeyAuth - see handlers.StatsStream and the
		// design doc §4.
		r.Group(func(r chi.Router) {
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/stream", handlers.StatsStream(db, hub))
		})
	})
}
