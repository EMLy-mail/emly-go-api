package clientws

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/presencehub"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// RegisterV2 mounts GET /v2/client/ws, the EMLy Updater's persistent
// presence channel
// (docs/superpowers/specs/2026-09-17-client-presence-ws-api-design.md).
// presence may be nil (tests, or a build that never constructs one);
// ClientWS and presencehub.Hub's own methods tolerate that.
func RegisterV2(r chi.Router, db *sqlx.DB, presence *presencehub.Hub) {
	r.Route("/client", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/ws", ClientWS(db, presence))
		})
	})
}
