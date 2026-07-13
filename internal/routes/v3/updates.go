package v3

import (
	apimw "emly-api-go/internal/middleware"
	"net/http"
	"time"

	"emly-api-go/internal/handlers"
	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/jmoiron/sqlx"
)

var validProducts = map[string]bool{"app": true, "updater": true}

// requireValidProduct rejects any {product} that isn't a known product before
// it reaches the handlers, so a typo'd product never silently falls through.
func requireValidProduct(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validProducts[chi.URLParam(r, "product")] {
			http.Error(w, `{"error":"product must be one of: app, updater"}`, http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func registerUpdates(r chi.Router, db *sqlx.DB, s3conn *storage.S3Connector, apiBaseURL, s3Prefix string) {
	r.Route("/updates/{product}", func(r chi.Router) {
		r.Use(requireValidProduct)

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
			r.Put("/releases/{version}", handlers.PutRelease(db))
			r.Patch("/releases/{version}", handlers.PatchRelease(db))
			r.Delete("/releases/{version}", handlers.DeleteRelease(db, s3conn, s3Prefix))
			r.Patch("/releases/{version}/channel", handlers.PatchReleaseChannel(db))
		})
	})
}
