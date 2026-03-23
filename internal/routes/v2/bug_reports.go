package v2

import (
	apimw "emly-api-go/internal/middleware"
	"time"

	"emly-api-go/internal/handlers"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/jmoiron/sqlx"
)

func registerBugReports(r chi.Router, db *sqlx.DB) {
	r.Route("/bug-report", func(r chi.Router) {
		// API key only: submit a report and check count
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/count", handlers.GetReportsCount(db))
			r.Post("/", handlers.CreateBugReport(db))
		})

		// API key + admin key: full read/write access
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/", handlers.GetAllBugReports(db))
			r.Get("/{id}", handlers.GetBugReportByID(db))
			r.Get("/{id}/status", handlers.GetReportStatusByID(db))
			r.Get("/{id}/files", handlers.GetReportFilesByReportID(db))
			r.Get("/{id}/files/{file_id}", handlers.GetReportFileByFileID(db))
			r.Get("/{id}/download", handlers.GetBugReportZipById(db))
			r.Patch("/{id}/status", handlers.PatchBugReportStatus(db))
			r.Delete("/{id}", handlers.DeleteBugReportByID(db))
		})
	})
}
