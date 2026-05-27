package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"text/template"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
	"emly-api-go/internal/storage"
	"emly-api-go/internal/timing"
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
	{"screenshot", models.FileRoleScreenshot, "image/png"},
	{"mail_file", models.FileRoleMailFile, "message/rfc822"},
	{"localstorage", models.FileRoleLocalStorage, "application/json"},
	{"config", models.FileRoleConfig, "application/json"},
}

func CreateBugReport(db *sqlx.DB, dbName string, s3conn *storage.S3Connector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
			return
		}
		timing.Mark(r.Context(), "parse_form")

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
			fmt.Sprintf("INSERT INTO %s.bug_reports (name, email, description, hwid, hostname, os_user, submitter_ip, system_info, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", dbName),
			name, email, description, hwid, hostname, osUser, submitterIP, systemInfo, models.BugReportStatusNew,
		)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		timing.Mark(r.Context(), "db_insert_report")

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
			defer func(file multipart.File) {
				err := file.Close()
				if err != nil {
					log.Fatalf("closing uploaded file failed: %v", err)
				}
			}(file)

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

			fileResult, err := db.ExecContext(r.Context(),
				fmt.Sprintf("INSERT INTO %s.bug_report_files (report_id, file_role, filename, mime_type, file_size, data) VALUES (?, ?, ?, ?, ?, ?)", dbName),
				reportID, fr.role, filename, mimeType, len(data), data,
			)
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			timing.Mark(r.Context(), "db_insert_file_"+string(fr.role))

			if s3conn != nil {
				fileID, err := fileResult.LastInsertId()
				if err != nil {
					log.Printf("[S3] could not get file insert id for report %d role %s: %v", reportID, fr.role, err)
				} else {
					s3Key := fmt.Sprintf("emly-api-files/bug-reports/%d/files/%s", reportID, filename)
					if _, err := s3conn.UploadFile(
						context.Background(), s3Key,
						bytes.NewReader(data), mimeType,
						map[string]string{"filename": filename, "id": strconv.FormatInt(fileID, 10)},
					); err != nil {
						log.Printf("[S3] upload failed for key %s: %v", s3Key, err)
					} else {
						timing.Mark(r.Context(), "s3_upload_file_"+string(fr.role))
					}
				}
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

func GetAllBugReports(db *sqlx.DB, dbName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, pageSize := 1, 20
		if p := r.URL.Query().Get("page"); p != "" {
			if v, err := strconv.Atoi(p); err == nil && v > 0 {
				page = v
			}
		}
		if ps := r.URL.Query().Get("page_size"); ps != "" {
			if v, err := strconv.Atoi(ps); err == nil && v > 0 && v <= 100 {
				pageSize = v
			}
		}

		search := r.URL.Query().Get("search")
		offset := (page - 1) * pageSize

		var conditions []string
		var params []interface{}

		if search != "" {
			like := "%" + search + "%"
			conditions = append(conditions, "(br.hostname LIKE ? OR br.os_user LIKE ? OR br.name LIKE ? OR br.email LIKE ?)")
			params = append(params, like, like, like, like)
		}

		whereClause := ""
		if len(conditions) > 0 {
			whereClause = "WHERE " + strings.Join(conditions, " AND ")
		}

		var total int
		countQuery := fmt.Sprintf("SELECT COUNT(*) FROM %s.bug_reports br ", dbName) + whereClause
		if err := db.GetContext(r.Context(), &total, countQuery, params...); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		timing.Mark(r.Context(), "db_count")

		mainQuery := fmt.Sprintf(`
	SELECT br.*, COUNT(bf.id) as file_count
	FROM %s.bug_reports br
	LEFT JOIN %s.bug_report_files bf ON bf.report_id = br.id
	`, dbName, dbName) + whereClause + `
	GROUP BY br.id
	ORDER BY br.created_at DESC
	LIMIT ? OFFSET ?`

		listParams := append(params, pageSize, offset)
		var reports []models.BugReportListItem
		if err := db.SelectContext(r.Context(), &reports, mainQuery, listParams...); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		timing.Mark(r.Context(), "db_select")

		jsonOK(w, map[string]interface{}{
			"data":        reports,
			"total":       total,
			"page":        page,
			"page_size":   pageSize,
			"total_pages": int(math.Ceil(float64(total) / float64(pageSize))),
		})
	}
}

func GetBugReportByID(db *sqlx.DB, dbName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			jsonError(w, http.StatusBadRequest, "missing id parameter")
			return
		}

		var report models.BugReport
		reportErr := db.GetContext(r.Context(), &report, fmt.Sprintf("SELECT * FROM %s.bug_reports WHERE id = ?", dbName), id)
		if errors.Is(reportErr, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "bug report not found")
			return
		}
		if reportErr != nil {
			jsonError(w, http.StatusInternalServerError, reportErr.Error())
			return
		}

		type response struct {
			Report models.BugReport `json:"report"`
		}

		responseData := response{
			Report: report,
		}

		jsonOK(w, responseData)
	}
}

func GetReportsCount(db *sqlx.DB, dbName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rawStatus := r.URL.Query().Get("status")

		query := fmt.Sprintf("SELECT COUNT(*) FROM %s.bug_reports", dbName)
		var args []interface{}

		if strings.TrimSpace(rawStatus) != "" {
			status, ok := models.ParseBugReportStatus(rawStatus)
			if !ok {
				jsonError(w, http.StatusBadRequest, "invalid status. allowed values: new, in_review, resolved, closed")
				return
			}
			query += " WHERE status = ?"
			args = append(args, status)
		}

		var count int
		if err := db.GetContext(r.Context(), &count, query, args...); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		jsonOK(w, map[string]int{"count": count})
	}
}

func GetReportFilesByReportID(db *sqlx.DB, dbName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			jsonError(w, http.StatusBadRequest, "missing id parameter")
			return
		}

		var files []models.BugReportFile
		if err := db.SelectContext(r.Context(), &files, fmt.Sprintf("SELECT * FROM %s.bug_report_files WHERE report_id = ?", dbName), id); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		jsonOK(w, files)
	}
}

func GetBugReportZipByID(db *sqlx.DB, dbName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			jsonError(w, http.StatusBadRequest, "missing id parameter")
			return
		}

		var report models.BugReport
		err := db.GetContext(r.Context(), &report, fmt.Sprintf("SELECT * FROM %s.bug_reports WHERE id = ?", dbName), id)
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "bug report not found")
			return
		}
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		timing.Mark(r.Context(), "db_fetch_report")

		var files []models.BugReportFile
		if err := db.SelectContext(r.Context(), &files, fmt.Sprintf("SELECT * FROM %s.bug_report_files WHERE report_id = ?", dbName), id); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		timing.Mark(r.Context(), "db_fetch_files")

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
		timing.Mark(r.Context(), "zip_build")

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"report-%d.zip\"", report.ID))
		_, err = w.Write(buf.Bytes())
		if err != nil {
			return
		}

	}
}

func GetReportFileByFileID(db *sqlx.DB, dbName string, s3conn *storage.S3Connector) http.HandlerFunc {
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

		var filename string
		if err := db.GetContext(r.Context(), &filename, fmt.Sprintf("SELECT filename FROM %s.bug_report_files WHERE report_id = ? AND id = ?", dbName), reportId, fileId); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		timing.Mark(r.Context(), "db_fetch_filename_by_id")

		// Try S3 first.
		if s3conn != nil {
			s3Key := fmt.Sprintf("emly-api-files/bug-reports/%s/files/%s", reportId, filename)
			rc, info, err := s3conn.GetFile(r.Context(), s3Key)
			if err == nil {
				defer rc.Close()
				timing.Mark(r.Context(), "s3_hit")
				log.Println("[S3] cache hit for key", s3Key)

				mimeType := info.ContentType
				if mimeType == "" {
					mimeType = "application/octet-stream"
				}
				filename := info.Metadata["filename"]
				if filename == "" {
					filename = fileId
				}
				w.Header().Set("Content-Type", mimeType)
				w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
				_, _ = io.Copy(w, rc)
				return
			}
			if storage.IsNotFound(err) {
				log.Printf("[S3] file %s not found on s3", fileId)
			}
			if !storage.IsNotFound(err) {
				log.Printf("[S3] unexpected error fetching key %s: %v", s3Key, err)
			}
		}

		// Fallback: query DB.
		var file models.BugReportFile
		err := db.GetContext(r.Context(), &file, fmt.Sprintf("SELECT filename, mime_type, data FROM %s.bug_report_files WHERE report_id = ? AND id = ?", dbName), reportId, fileId)
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "file not found")
			return
		}
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		timing.Mark(r.Context(), "db_select")

		// Lazy-upload to S3 so future requests are served from there.
		if s3conn != nil {
			s3Key := fmt.Sprintf("emly-api-files/bug-reports/%s/files/%s", reportId, fileId)
			dataCopy := make([]byte, len(file.Data))
			copy(dataCopy, file.Data)
			mime := file.MimeType
			fname := file.Filename
			go func() {
				if _, err := s3conn.UploadFile(
					context.Background(), s3Key,
					bytes.NewReader(dataCopy), mime,
					map[string]string{"filename": fname},
				); err != nil {
					log.Printf("[S3] lazy upload failed for key %s: %v", s3Key, err)
				}
			}()
		}

		mimeType := file.MimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", mimeType)
		w.Header().Set("Content-Disposition", "attachment; filename=\""+file.Filename+"\"")
		_, _ = w.Write(file.Data)
	}
}

func GetReportStatusByID(db *sqlx.DB, dbName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reportId := chi.URLParam(r, "id")
		if reportId == "" {
			jsonError(w, http.StatusBadRequest, "missing report id parameter")
			return
		}

		var reportStatus models.BugReportStatus
		if err := db.GetContext(r.Context(), &reportStatus, fmt.Sprintf("SELECT status FROM %s.bug_reports WHERE id = ?", dbName), reportId); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		jsonOK(w, map[string]string{"status": string(reportStatus)})
	}
}

func PatchBugReportStatus(db *sqlx.DB, dbName string) http.HandlerFunc {
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

		result, err := db.ExecContext(r.Context(), fmt.Sprintf("UPDATE %s.bug_reports SET status = ? WHERE id = ?", dbName), reportStatus, reportId)
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

func DeleteBugReportByID(db *sqlx.DB, dbName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reportId := chi.URLParam(r, "id")
		if reportId == "" {
			jsonError(w, http.StatusBadRequest, "missing report id parameter")
			return
		}

		result, err := db.ExecContext(r.Context(), fmt.Sprintf("DELETE FROM %s.bug_reports WHERE id = ?", dbName), reportId)
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
