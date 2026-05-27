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

func registerBugReports(r chi.Router, db *sqlx.DB, dbName string, s3conn *storage.S3Connector) {
	r.Route("/bug-report", func(r chi.Router) {
		// API key only: submit a report and check count
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/count", handlers.GetReportsCount(db, dbName))
			r.Post("/", handlers.CreateBugReport(db, dbName, s3conn))
		})

		// API key + admin key: full read/write access
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(httprate.LimitByIP(30, time.Minute))

			r.Get("/", handlers.GetAllBugReports(db, dbName))
			r.Get("/{id}", handlers.GetBugReportByID(db, dbName))
			r.Get("/{id}/status", handlers.GetReportStatusByID(db, dbName))
			r.Get("/{id}/files", handlers.GetReportFilesByReportID(db, dbName))
			r.Get("/{id}/files/{file_id}", handlers.GetReportFileByFileID(db, dbName, s3conn))
			r.Get("/{id}/download", handlers.GetBugReportZipByID(db, dbName))
			r.Patch("/{id}/status", handlers.PatchBugReportStatus(db, dbName))
			r.Delete("/{id}", handlers.DeleteBugReportByID(db, dbName))
		})
	})
}
