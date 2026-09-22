package v1

import (
	apimw "emly-api-go/internal/middleware"
	"time"

	"emly-api-go/internal/bugreports"
	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func registerBugReports(r chi.Router, db *sqlx.DB, dbName string, s3conn *storage.S3Connector) {
	r.Route("/bug-reports", func(r chi.Router) {
		// API key only: submit a report and check count
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/count", bugreports.GetReportsCount(db, dbName))
			r.Post("/", bugreports.CreateBugReport(db, dbName, s3conn))
		})

		// API key + admin key: full read/write access
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/", bugreports.GetAllBugReports(db, dbName))
			r.Get("/{id}", bugreports.GetBugReportByID(db, dbName))
			r.Get("/{id}/status", bugreports.GetReportStatusByID(db, dbName))
			r.Get("/{id}/files", bugreports.GetReportFilesByReportID(db, dbName))
			r.Get("/{id}/files/{file_id}", bugreports.GetReportFileByFileID(db, dbName, s3conn))
			r.Get("/{id}/download", bugreports.GetBugReportZipByID(db, dbName))
			r.Patch("/{id}/status", bugreports.PatchBugReportStatus(db, dbName))
			r.Delete("/{id}", bugreports.DeleteBugReportByID(db, dbName))
		})
	})
}
