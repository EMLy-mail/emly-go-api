package bugreports

import (
	"time"

	apimw "emly-api-go/internal/middleware"

	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func RegisterV2(r chi.Router, db *sqlx.DB, dbName string, s3conn *storage.S3Connector) {
	r.Route("/bug-report", func(r chi.Router) {
		// API key only: submit a report and check count
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/count", GetReportsCount(db, dbName))
			r.Post("/", CreateBugReport(db, dbName, s3conn))
		})

		// API key + admin key: full read/write access
		r.Group(func(r chi.Router) {
			r.Use(apimw.APIKeyAuth(db))
			r.Use(apimw.AdminKeyAuth(db))
			r.Use(apimw.RouteLimitByIP(30, time.Minute))

			r.Get("/", GetAllBugReports(db, dbName))
			r.Get("/{id}", GetBugReportByID(db, dbName))
			r.Get("/{id}/status", GetReportStatusByID(db, dbName))
			r.Get("/{id}/files", GetReportFilesByReportID(db, dbName))
			r.Get("/{id}/files/{file_id}", GetReportFileByFileID(db, dbName, s3conn))
			r.Get("/{id}/download", GetBugReportZipByID(db, dbName))
			r.Patch("/{id}/status", PatchBugReportStatus(db, dbName))
			r.Delete("/{id}", DeleteBugReportByID(db, dbName))
		})
	})
}
