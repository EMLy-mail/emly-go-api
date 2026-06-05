package v2

import (
	apimw "emly-api-go/internal/middleware"
	"time"

	"emly-api-go/internal/handlers"
	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/jmoiron/sqlx"
)

func registerUpdates(r chi.Router, db *sqlx.DB, s3conn *storage.S3Connector, apiBaseURL, s3Prefix string) {
	r.Route("/updates", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(httprate.LimitByIP(30, time.Minute))
			r.Get("/manifest", handlers.GetUpdateManifest(db, apiBaseURL))
			r.Get("/releases/{version}/download", handlers.DownloadRelease(db, s3conn, s3Prefix))
		})

		r.Group(func(r chi.Router) {
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/releases", handlers.ListReleases(db))
			r.Post("/releases", handlers.CreateRelease(db, s3conn, s3Prefix))
			r.Delete("/releases/{version}", handlers.DeleteRelease(db, s3conn, s3Prefix))
			r.Patch("/releases/{version}/channel", handlers.PatchReleaseChannel(db))
		})
	})
}
