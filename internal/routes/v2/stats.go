package v2

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/handlers"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/jmoiron/sqlx"
)

func registerStats(r chi.Router, db *sqlx.DB) {
	r.Route("/stats", func(r chi.Router) {
		r.Use(apimw.AdminKeyAuth(db))
		r.Use(httprate.LimitByIP(30, time.Minute))

		r.Get("/summary", handlers.GetStatsSummary(db))
		r.Get("/clients", handlers.ListStatsClients(db))
		r.Get("/clients/{id}", handlers.GetStatsClientDetail(db))
		r.Get("/events", handlers.GetStatsEvents(db))
	})
}
