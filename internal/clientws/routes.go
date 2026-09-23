package clientws

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/clienthub"
	"emly-api-go/internal/presencehub"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// RegisterV2 mounts GET /v2/client/ws, the EMLy Updater's persistent
// presence channel
// (docs/superpowers/specs/2026-09-17-client-presence-ws-api-design.md).
// presence may be nil (tests, or a build that never constructs one);
// ClientWS and presencehub.Hub's own methods tolerate that. hub is the
// protocol v2 state (internal/clienthub); nil is likewise tolerated.
func RegisterV2(r chi.Router, db *sqlx.DB, presence *presencehub.Hub, hub *clienthub.Hub) {
	r.Route("/client", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/ws", ClientWS(db, presence, hub))
		})

		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))
			mountAdmin(r, hub)
		})
	})
}
