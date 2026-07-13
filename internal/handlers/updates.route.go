package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
	"emly-api-go/internal/storage"
	"emly-api-go/internal/timing"
)

var validChannels = map[string]bool{"stable": true, "beta": true, "archived": true}
var validSeverity = map[string]bool{"none": true, "security": true, "bugfix": true, "feature": true}
var validProducts = map[string]bool{"app": true, "updater": true}

const releaseSelectCols = `
	id, product, version, channel, download_filename, sha256_checksum, short_note,
	severity_type, description_en, description_it, is_critical, critical_version, min_required_version,
	released_at, created_at `

// productParam resolves the target product for a request. Routes mounted under
// /v3/updates/{product}/... carry it as a URL param; legacy /v2/updates/...
// routes have no {product} segment and always mean the EMLy app.
func productParam(r *http.Request) string {
	if p := chi.URLParam(r, "product"); p != "" {
		return p
	}
	return "app"
}

// downloadPathPrefix returns the release-download path prefix matching how this
// request reached the handler, so manifest download links stay on the same API
// version the client used (v2 clients keep getting v2 links, v3 clients get v3).
func downloadPathPrefix(r *http.Request, product string) string {
	if chi.URLParam(r, "product") != "" {
		return "v3/updates/" + product + "/releases"
	}
	return "v2/updates/releases"
}

func GetUpdateManifest(db *sqlx.DB, s3BaseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		product := productParam(r)

		var releases []models.Release
		err := db.SelectContext(r.Context(), &releases,
			`SELECT`+releaseSelectCols+`FROM update_releases WHERE product = ? ORDER BY released_at DESC`,
			product)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch releases")
			return
		}
		timing.Mark(r.Context(), "db_select")
		manifest := buildManifest(releases, s3BaseURL, downloadPathPrefix(r, product))
		timing.Mark(r.Context(), "build_manifest")
		jsonOK(w, manifest)
	}
}

func ListReleases(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		product := productParam(r)
		channel := r.URL.Query().Get("channel")

		var releases []models.Release
		var err error
		if channel != "" {
			err = db.SelectContext(r.Context(), &releases,
				`SELECT`+releaseSelectCols+`FROM update_releases WHERE product = ? AND channel = ? ORDER BY released_at DESC`,
				product, channel)
		} else {
			err = db.SelectContext(r.Context(), &releases,
				`SELECT`+releaseSelectCols+`FROM update_releases WHERE product = ? ORDER BY released_at DESC`,
				product)
		}
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch releases")
			return
		}
		jsonOK(w, releases)
	}
}

func s3Key(prefix, filename string) string {
	if prefix == "" {
		return filename
	}
	return prefix + "/" + filename
}

// CreateRelease handles POST .../updates[/{product}]/releases as multipart/form-data.
// The .exe is uploaded to R2; SHA-256 is computed server-side.
func CreateRelease(db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s3conn == nil {
			jsonError(w, http.StatusServiceUnavailable, "S3 storage is not configured")
			return
		}

		product := productParam(r)
		if !validProducts[product] {
			jsonError(w, http.StatusBadRequest, "product must be one of: app, updater")
			return
		}

		if err := r.ParseMultipartForm(32 << 20); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
			return
		}

		version := strings.TrimSpace(r.FormValue("version"))
		channel := strings.TrimSpace(r.FormValue("channel"))
		shortNote := r.FormValue("short_note")
		severityType := strings.TrimSpace(r.FormValue("severity_type"))
		descEN := strings.TrimSpace(r.FormValue("description_en"))
		descIT := strings.TrimSpace(r.FormValue("description_it"))
		isCritical := r.FormValue("is_critical") == "true" || r.FormValue("is_critical") == "1"
		criticalVer := strings.TrimSpace(r.FormValue("critical_version"))
		minVer := strings.TrimSpace(r.FormValue("min_required_version"))
		releasedAtStr := strings.TrimSpace(r.FormValue("released_at"))

		if version == "" {
			jsonError(w, http.StatusBadRequest, "version is required")
			return
		}
		if channel == "" {
			channel = "archived"
		}
		if !validChannels[channel] {
			jsonError(w, http.StatusBadRequest, "channel must be one of: stable, beta, archived")
			return
		}
		if severityType == "" {
			severityType = "none"
		}
		if !validSeverity[severityType] {
			jsonError(w, http.StatusBadRequest, "severity_type must be one of: none, security, bugfix, feature")
			return
		}

		releasedAt := time.Now().UTC()
		if releasedAtStr != "" {
			if t, err := time.Parse(time.RFC3339, releasedAtStr); err == nil {
				releasedAt = t
			}
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			jsonError(w, http.StatusBadRequest, "missing file field")
			return
		}
		defer file.Close()

		data, err := io.ReadAll(file)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to read file: "+err.Error())
			return
		}

		sum := sha256.Sum256(data)
		checksum := hex.EncodeToString(sum[:])
		filename := header.Filename

		if _, err := s3conn.UploadFile(r.Context(), s3Key(s3Prefix, filename), bytes.NewReader(data), "application/octet-stream", nil); err != nil {
			jsonError(w, http.StatusInternalServerError, "upload failed: "+err.Error())
			return
		}

		var pDescEN, pDescIT, pCriticalVer, pMinVer *string
		if descEN != "" {
			pDescEN = &descEN
		}
		if descIT != "" {
			pDescIT = &descIT
		}
		if criticalVer != "" {
			pCriticalVer = &criticalVer
		}
		if minVer != "" {
			pMinVer = &minVer
		}

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback()

		if isCritical {
			if _, err = tx.ExecContext(r.Context(),
				`UPDATE update_releases SET is_critical = 0, critical_version = NULL WHERE product = ? AND is_critical = 1`,
				product,
			); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing critical flag")
				return
			}
		}

		_, err = tx.ExecContext(r.Context(),
			`INSERT INTO update_releases
			 (product, version, channel, download_filename, sha256_checksum, short_note, severity_type,
			  description_en, description_it, is_critical, critical_version, min_required_version, released_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			product, version, channel, filename, checksum, shortNote,
			severityType, pDescEN, pDescIT, isCritical, pCriticalVer, pMinVer, releasedAt,
		)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to create release: "+err.Error())
			return
		}

		if err := tx.Commit(); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		jsonCreated(w, map[string]string{
			"product":           product,
			"version":           version,
			"channel":           channel,
			"download_filename": filename,
			"sha256_checksum":   checksum,
		})
	}
}

func DownloadRelease(db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s3conn == nil {
			jsonError(w, http.StatusServiceUnavailable, "S3 storage is not configured")
			return
		}

		product := productParam(r)
		version := chi.URLParam(r, "version")

		var filename string
		if err := db.GetContext(r.Context(), &filename,
			`SELECT download_filename FROM update_releases WHERE product = ? AND version = ?`, product, version); err != nil {
			jsonError(w, http.StatusNotFound, "release not found")
			return
		}

		rc, info, err := s3conn.GetFile(r.Context(), s3Key(s3Prefix, filename))
		if err != nil {
			if storage.IsNotFound(err) {
				jsonError(w, http.StatusNotFound, "installer file not found in storage")
				return
			}
			jsonError(w, http.StatusInternalServerError, "failed to retrieve file: "+err.Error())
			return
		}
		defer rc.Close()

		contentType := info.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		if info.Size > 0 {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
		}

		io.Copy(w, rc) //nolint:errcheck
	}
}

func DeleteRelease(db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		product := productParam(r)
		version := chi.URLParam(r, "version")

		var filename string
		err := db.GetContext(r.Context(), &filename,
			`SELECT download_filename FROM update_releases WHERE product = ? AND version = ?`, product, version)
		if err != nil {
			jsonError(w, http.StatusNotFound, "release not found")
			return
		}

		if s3conn != nil {
			if err := s3conn.DeleteFile(r.Context(), s3Key(s3Prefix, filename)); err != nil && !storage.IsNotFound(err) {
				jsonError(w, http.StatusInternalServerError, "failed to delete file from storage: "+err.Error())
				return
			}
		}

		res, err := db.ExecContext(r.Context(),
			`DELETE FROM update_releases WHERE product = ? AND version = ?`, product, version)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to delete release: "+err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			jsonError(w, http.StatusNotFound, "release not found")
			return
		}

		jsonOK(w, map[string]bool{"deleted": true})
	}
}

type patchChannelRequest struct {
	Channel string `json:"channel"`
}

type putReleaseRequest struct {
	Channel            string  `json:"channel"`
	ShortNote          string  `json:"short_note"`
	SeverityType       string  `json:"severity_type"`
	DescriptionEN      *string `json:"description_en"`
	DescriptionIT      *string `json:"description_it"`
	IsCritical         bool    `json:"is_critical"`
	CriticalVersion    *string `json:"critical_version"`
	MinRequiredVersion *string `json:"min_required_version"`
	ReleasedAt         string  `json:"released_at"`
}

type patchReleaseRequest struct {
	Channel            *string `json:"channel"`
	ShortNote          *string `json:"short_note"`
	SeverityType       *string `json:"severity_type"`
	DescriptionEN      *string `json:"description_en"`
	DescriptionIT      *string `json:"description_it"`
	IsCritical         *bool   `json:"is_critical"`
	CriticalVersion    *string `json:"critical_version"`
	MinRequiredVersion *string `json:"min_required_version"`
	ReleasedAt         *string `json:"released_at"`
}

func PatchReleaseChannel(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		product := productParam(r)
		version := chi.URLParam(r, "version")

		var req patchChannelRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if !validChannels[req.Channel] {
			jsonError(w, http.StatusBadRequest, "channel must be one of: stable, beta, archived")
			return
		}

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback()

		// Archive whoever currently holds the target channel slot for this product
		if req.Channel != "archived" {
			if _, err = tx.ExecContext(r.Context(),
				`UPDATE update_releases SET channel = 'archived' WHERE product = ? AND channel = ? AND version != ?`,
				product, req.Channel, version,
			); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to archive existing release")
				return
			}
		}

		res, err := tx.ExecContext(r.Context(),
			`UPDATE update_releases SET channel = ? WHERE product = ? AND version = ?`, req.Channel, product, version)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to update channel")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			jsonError(w, http.StatusNotFound, "release not found")
			return
		}

		if err := tx.Commit(); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		jsonOK(w, map[string]string{"product": product, "version": version, "channel": req.Channel})
	}
}

func PutRelease(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		product := productParam(r)
		version := chi.URLParam(r, "version")

		var req putReleaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		if req.Channel == "" {
			req.Channel = "archived"
		}
		if !validChannels[req.Channel] {
			jsonError(w, http.StatusBadRequest, "channel must be one of: stable, beta, archived")
			return
		}
		if req.SeverityType == "" {
			req.SeverityType = "none"
		}
		if !validSeverity[req.SeverityType] {
			jsonError(w, http.StatusBadRequest, "severity_type must be one of: none, security, bugfix, feature")
			return
		}

		releasedAt := time.Now().UTC()
		if req.ReleasedAt != "" {
			if t, err := time.Parse(time.RFC3339, req.ReleasedAt); err == nil {
				releasedAt = t.UTC()
			}
		}

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback()

		if req.Channel != "archived" {
			if _, err = tx.ExecContext(r.Context(),
				`UPDATE update_releases SET channel = 'archived' WHERE product = ? AND channel = ? AND version != ?`,
				product, req.Channel, version,
			); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to archive existing release")
				return
			}
		}

		if req.IsCritical {
			if _, err = tx.ExecContext(r.Context(),
				`UPDATE update_releases SET is_critical = 0, critical_version = NULL WHERE product = ? AND is_critical = 1 AND version != ?`,
				product, version,
			); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing critical flag")
				return
			}
		}

		res, err := tx.ExecContext(r.Context(),
			`UPDATE update_releases
			 SET channel = ?, short_note = ?, severity_type = ?,
			     description_en = ?, description_it = ?, is_critical = ?, critical_version = ?,
			     min_required_version = ?, released_at = ?
			 WHERE product = ? AND version = ?`,
			req.Channel, req.ShortNote, req.SeverityType,
			req.DescriptionEN, req.DescriptionIT, req.IsCritical, req.CriticalVersion,
			req.MinRequiredVersion, releasedAt, product, version,
		)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to update release")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			jsonError(w, http.StatusNotFound, "release not found")
			return
		}

		if err := tx.Commit(); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		var updated models.Release
		if err := db.GetContext(r.Context(), &updated,
			`SELECT`+releaseSelectCols+`FROM update_releases WHERE product = ? AND version = ?`, product, version,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch updated release")
			return
		}
		jsonOK(w, updated)
	}
}

func PatchRelease(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		product := productParam(r)
		version := chi.URLParam(r, "version")

		var req patchReleaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		if req.Channel != nil && !validChannels[*req.Channel] {
			jsonError(w, http.StatusBadRequest, "channel must be one of: stable, beta, archived")
			return
		}
		if req.SeverityType != nil && !validSeverity[*req.SeverityType] {
			jsonError(w, http.StatusBadRequest, "severity_type must be one of: none, security, bugfix, feature")
			return
		}

		var setClauses []string
		var args []interface{}

		if req.Channel != nil {
			setClauses = append(setClauses, "channel = ?")
			args = append(args, *req.Channel)
		}
		if req.ShortNote != nil {
			setClauses = append(setClauses, "short_note = ?")
			args = append(args, *req.ShortNote)
		}
		if req.SeverityType != nil {
			setClauses = append(setClauses, "severity_type = ?")
			args = append(args, *req.SeverityType)
		}
		if req.DescriptionEN != nil {
			setClauses = append(setClauses, "description_en = ?")
			args = append(args, *req.DescriptionEN)
		}
		if req.DescriptionIT != nil {
			setClauses = append(setClauses, "description_it = ?")
			args = append(args, *req.DescriptionIT)
		}
		if req.IsCritical != nil {
			setClauses = append(setClauses, "is_critical = ?")
			args = append(args, *req.IsCritical)
		}
		if req.CriticalVersion != nil {
			setClauses = append(setClauses, "critical_version = ?")
			args = append(args, *req.CriticalVersion)
		}
		if req.MinRequiredVersion != nil {
			setClauses = append(setClauses, "min_required_version = ?")
			args = append(args, *req.MinRequiredVersion)
		}
		if req.ReleasedAt != nil {
			t, err := time.Parse(time.RFC3339, *req.ReleasedAt)
			if err != nil {
				jsonError(w, http.StatusBadRequest, "released_at must be RFC3339")
				return
			}
			setClauses = append(setClauses, "released_at = ?")
			args = append(args, t.UTC())
		}

		if len(setClauses) == 0 {
			jsonError(w, http.StatusBadRequest, "no fields to update")
			return
		}

		args = append(args, product, version)

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback()

		if req.Channel != nil && *req.Channel != "archived" {
			if _, err = tx.ExecContext(r.Context(),
				`UPDATE update_releases SET channel = 'archived' WHERE product = ? AND channel = ? AND version != ?`,
				product, *req.Channel, version,
			); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to archive existing release")
				return
			}
		}

		if req.IsCritical != nil && *req.IsCritical {
			if _, err = tx.ExecContext(r.Context(),
				`UPDATE update_releases SET is_critical = 0, critical_version = NULL WHERE product = ? AND is_critical = 1 AND version != ?`,
				product, version,
			); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing critical flag")
				return
			}
		}

		query := "UPDATE update_releases SET " + strings.Join(setClauses, ", ") + " WHERE product = ? AND version = ?"
		res, err := tx.ExecContext(r.Context(), query, args...)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to update release")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			jsonError(w, http.StatusNotFound, "release not found")
			return
		}

		if err := tx.Commit(); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		var updated models.Release
		if err := db.GetContext(r.Context(), &updated,
			`SELECT`+releaseSelectCols+`FROM update_releases WHERE product = ? AND version = ?`, product, version,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch updated release")
			return
		}
		jsonOK(w, updated)
	}
}

func buildManifest(releases []models.Release, apiBaseURL, downloadPathPrefix string) models.UpdateManifest {
	m := models.UpdateManifest{
		SHA256Checksums:      make(map[string]string),
		ReleaseNotes:         make(map[string]string),
		DetailedReleaseNotes: make(map[string]models.DetailedNote),
	}

	for _, rel := range releases {
		if rel.SHA256Checksum != "" {
			m.SHA256Checksums[rel.Version] = rel.SHA256Checksum
		}
		if rel.ShortNote != "" {
			m.ReleaseNotes[rel.Version] = rel.ShortNote
		}
		if rel.SeverityType != "none" {
			note := models.DetailedNote{
				SeverityType: rel.SeverityType,
				Description:  make(map[string]string),
			}
			if rel.DescriptionEN != nil {
				note.Description["en"] = *rel.DescriptionEN
			}
			if rel.DescriptionIT != nil {
				note.Description["it"] = *rel.DescriptionIT
			}
			m.DetailedReleaseNotes[rel.Version] = note
		}

		if rel.IsCritical {
			m.IsCritical = true
			if rel.CriticalVersion != nil {
				m.CriticalVersion = *rel.CriticalVersion
			} else {
				m.CriticalVersion = rel.Version
			}
		}

		switch rel.Channel {
		case "stable":
			m.StableVersion = rel.Version
			m.StableDownload = fmt.Sprintf("%s/%s/%s/download", apiBaseURL, downloadPathPrefix, rel.Version)
			if rel.MinRequiredVersion != nil {
				m.MinRequiredVersion = *rel.MinRequiredVersion
			}
		case "beta":
			m.BetaVersion = rel.Version
			m.BetaDownload = fmt.Sprintf("%s/%s/%s/download", apiBaseURL, downloadPathPrefix, rel.Version)
		}
	}

	if len(m.DetailedReleaseNotes) == 0 {
		m.DetailedReleaseNotes = nil
	}

	return m
}
