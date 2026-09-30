package products

import (
	"time"

	apimw "emly-api-go/internal/middleware"
	"emly-api-go/internal/productreg"
	"emly-api-go/internal/session"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// RegisterV2 mounts the product registry's admin surface. Admin-key gated
// like release management: a product decides what the fleet can be offered.
//
// A dashboard user (X-Session-Token) only sees and edits the products
// assigned to them (session.Scope); creating one assigns it to its creator.
//
// reg may be nil in tests; writes then land in the table and nothing is
// refreshed.
func RegisterV2(r chi.Router, db *sqlx.DB, reg *productreg.Registry) {
	r.Route("/products", func(r chi.Router) {
		r.Use(apimw.AdminKeyAuth(db))
		r.Use(apimw.RouteLimitByIP(30, time.Minute))
		r.Use(session.LoadScope(db))

		r.Get("/", ListProducts(db))
		r.Post("/", CreateProduct(db, reg))
		r.Get("/{slug}", GetProduct(db))
		r.Patch("/{slug}", PatchProduct(db, reg))
		r.Delete("/{slug}", DeleteProduct(db, reg))
	})
}
