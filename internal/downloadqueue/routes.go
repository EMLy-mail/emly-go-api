package downloadqueue

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// RegisterV2 mounts the download queue's admin surface. It needs both the
// admin key and the dashboard key: the queue is operated from the dashboard,
// and changing it decides whether the whole fleet can fetch an installer.
// The queue itself is applied to the download routes by updates.RegisterV2.
//
// q may be nil (tests); every route then answers 503.
func RegisterV2(r chi.Router, db *sqlx.DB, q *Queue) {
	r.Route("/download-queue", func(r chi.Router) {
		r.Use(apimw.AdminKeyAuth(db))
		r.Use(apimw.DashboardKeyAuth())
		r.Use(apimw.RouteLimitByIP(30, time.Minute))

		r.Get("/", GetQueue(q))
		r.Patch("/", PatchQueue(db, q))
		r.Post("/reset", ResetQueue(db, q))
		r.Delete("/slots", EvictAllSlots(db, q))
		r.Delete("/slots/{id}", EvictSlot(db, q))
	})
}
