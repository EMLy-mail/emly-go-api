package handlers

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"text/template"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
)

//go:embed templates/report.txt.tmpl
var reportTemplateFS embed.FS

var reportTmpl = template.Must(
	template.ParseFS(reportTemplateFS, "templates/report.txt.tmpl"),
)

var fileRoles = []struct {
	field       string
	role        models.FileRole
	defaultMime string
}{
	{"attachment", models.FileRoleAttachment, "application/octet-stream"},
	{"screenshot", models.FileRoleScreenshot, "image/png"},
	{"log", models.FileRoleLog, "text/plain"},
}

func CreateBugReport(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
			return
		}

		name := r.FormValue("name")
		email := r.FormValue("email")
		description := r.FormValue("description")
		hwid := r.FormValue("hwid")
		hostname := r.FormValue("hostname")
		osUser := r.FormValue("os_user")
		systemInfoStr := r.FormValue("system_info")

		if name == "" || email == "" || description == "" {
			jsonError(w, http.StatusBadRequest, "name, email and description are required")
			return
		}

		submitterIP := strings.TrimSpace(strings.SplitN(r.Header.Get("X-Forwarded-For"), ",", 2)[0])
		if submitterIP == "" {
			submitterIP = r.Header.Get("X-Real-IP")
		}
		if submitterIP == "" {
			submitterIP = "unknown"
		}

		var systemInfo json.RawMessage
		if systemInfoStr != "" && json.Valid([]byte(systemInfoStr)) {
			systemInfo = json.RawMessage(systemInfoStr)
		}

		log.Printf("[BUGREPORT] Received from name=%s hwid=%s ip=%s", name, hwid, submitterIP)

		result, err := db.ExecContext(r.Context(),
			"INSERT INTO emly_bugreports_dev.bug_reports (name, email, description, hwid, hostname, os_user, submitter_ip, system_info, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			name, email, description, hwid, hostname, osUser, submitterIP, systemInfo, models.BugReportStatusNew,
		)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		reportID, err := result.LastInsertId()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		for _, fr := range fileRoles {
			file, header, err := r.FormFile(fr.field)
			if err != nil {
				continue
			}
			defer file.Close()

			data, err := io.ReadAll(file)
			if err != nil {
				jsonError(w, http.StatusInternalServerError, "reading file "+fr.field+": "+err.Error())
				return
			}

			mimeType := header.Header.Get("Content-Type")
			if mimeType == "" {
				mimeType = fr.defaultMime
			}
			filename := header.Filename
			if filename == "" {
				filename = fr.field + ".bin"
			}

			log.Printf("[BUGREPORT] File uploaded: role=%s size=%d bytes", fr.role, len(data))

			_, err = db.ExecContext(r.Context(),
				"INSERT INTO emly_bugreports_dev.bug_report_files (report_id, file_role, filename, mime_type, file_size, data) VALUES (?, ?, ?, ?, ?, ?)",
				reportID, fr.role, filename, mimeType, len(data), data,
			)
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}

		log.Printf("[BUGREPORT] Created successfully with id=%d", reportID)

		jsonCreated(w, map[string]interface{}{
			"success":   true,
			"report_id": reportID,
			"message":   "Bug report submitted successfully",
		})
	}
}

func GetAllBugReports(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var reports []models.BugReport
		if err := db.SelectContext(r.Context(), &reports, "SELECT * FROM emly_bugreports_dev.bug_reports"); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		jsonOK(w, reports)
	}
}

func GetBugReportByID(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			jsonError(w, http.StatusBadRequest, "missing id parameter")
			return
		}

		var report models.BugReport
		err := db.GetContext(r.Context(), &report, "SELECT * FROM emly_bugreports_dev.bug_reports WHERE id = ?", id)
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "bug report not found")
			return
		}
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		jsonOK(w, report)
	}
}

func GetReportsCount(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var count int
		if err := db.GetContext(r.Context(), &count, "SELECT COUNT(*) FROM emly_bugreports_dev.bug_reports"); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		jsonOK(w, map[string]int{"count": count})
	}
}

func GetReportFilesByReportID(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			jsonError(w, http.StatusBadRequest, "missing id parameter")
			return
		}

		var files []models.BugReportFile
		if err := db.SelectContext(r.Context(), &files, "SELECT * FROM emly_bugreports_dev.bug_report_files WHERE report_id = ?", id); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		jsonOK(w, files)
	}
}

func GetBugReportZipById(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			jsonError(w, http.StatusBadRequest, "missing id parameter")
			return
		}

		var report models.BugReport
		err := db.GetContext(r.Context(), &report, "SELECT * FROM emly_bugreports_dev.bug_reports WHERE id = ?", id)
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "bug report not found")
			return
		}
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		var files []models.BugReportFile
		if err := db.SelectContext(r.Context(), &files, "SELECT * FROM emly_bugreports_dev.bug_report_files WHERE report_id = ?", id); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		var sysInfoStr string
		if len(report.SystemInfo) > 0 && string(report.SystemInfo) != "null" {
			if pretty, err := json.MarshalIndent(report.SystemInfo, "", "  "); err == nil {
				sysInfoStr = string(pretty)
			}
		}

		tmplData := struct {
			models.BugReport
			CreatedAt  string
			UpdatedAt  string
			SystemInfo string
		}{
			BugReport:  report,
			CreatedAt:  report.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			UpdatedAt:  report.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			SystemInfo: sysInfoStr,
		}

		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)

		rf, err := zw.Create("report.txt")
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err = reportTmpl.Execute(rf, tmplData); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		for _, file := range files {
			ff, err := zw.Create(fmt.Sprintf("%s/%s", file.FileRole, file.Filename))
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if _, err = ff.Write(file.Data); err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}

		if err := zw.Close(); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"report-%d.zip\"", report.ID))
		w.Write(buf.Bytes())
	}
}

func GetReportFileByFileID(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reportId := chi.URLParam(r, "id")
		if reportId == "" {
			jsonError(w, http.StatusBadRequest, "missing report id parameter")
			return
		}
		fileId := chi.URLParam(r, "file_id")
		if fileId == "" {
			jsonError(w, http.StatusBadRequest, "missing file id parameter")
			return
		}

		var file models.BugReportFile
		err := db.GetContext(r.Context(), &file, "SELECT filename, mime_type, data FROM emly_bugreports_dev.bug_report_files WHERE report_id = ? AND id = ?", reportId, fileId)
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "file not found")
			return
		}
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		mimeType := file.MimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", mimeType)
		w.Header().Set("Content-Disposition", "attachment; filename=\""+file.Filename+"\"")
		_, err = w.Write(file.Data)
		if err != nil {
			return
		}
	}
}

func GetReportStatusByID(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reportId := chi.URLParam(r, "id")
		if reportId == "" {
			jsonError(w, http.StatusBadRequest, "missing report id parameter")
			return
		}

		var reportStatus models.BugReportStatus
		if err := db.GetContext(r.Context(), &reportStatus, "SELECT status FROM emly_bugreports_dev.bug_reports WHERE id = ?", reportId); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		jsonOK(w, map[string]string{"status": string(reportStatus)})
	}
}

func PatchBugReportStatus(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reportId := chi.URLParam(r, "id")
		if reportId == "" {
			jsonError(w, http.StatusBadRequest, "missing report id parameter")
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "unable to read request body: "+err.Error())
			return
		}
		reportStatus := models.BugReportStatus(body)

		result, err := db.ExecContext(r.Context(), "UPDATE emly_bugreports_dev.bug_reports SET status = ? WHERE id = ?", reportStatus, reportId)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rowsAffected == 0 {
			jsonError(w, http.StatusNotFound, "bug report not found")
			return
		}

		jsonOK(w, map[string]string{"message": "status updated successfully"})
	}
}

func DeleteBugReportByID(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reportId := chi.URLParam(r, "id")
		if reportId == "" {
			jsonError(w, http.StatusBadRequest, "missing report id parameter")
			return
		}

		result, err := db.ExecContext(r.Context(), "DELETE FROM emly_bugreports_dev.bug_reports WHERE id = ?", reportId)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rowsAffected == 0 {
			jsonError(w, http.StatusNotFound, "bug report not found")
			return
		}

		jsonOK(w, map[string]string{"message": "bug report deleted successfully"})
	}
}
