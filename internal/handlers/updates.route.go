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
)

var validChannels = map[string]bool{"stable": true, "beta": true, "archived": true}
var validSeverity = map[string]bool{"none": true, "security": true, "bugfix": true, "feature": true}

const releaseSelectCols = `
	id, version, channel, download_filename, sha256_checksum, short_note,
	severity_type, description_en, description_it, is_critical, min_required_version,
	released_at, created_at `

func GetUpdateManifest(db *sqlx.DB, s3BaseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var releases []models.Release
		err := db.SelectContext(r.Context(), &releases,
			`SELECT`+releaseSelectCols+`FROM update_releases ORDER BY released_at DESC`)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch releases")
			return
		}
		jsonOK(w, buildManifest(releases, s3BaseURL))
	}
}

func ListReleases(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := r.URL.Query().Get("channel")

		var releases []models.Release
		var err error
		if channel != "" {
			err = db.SelectContext(r.Context(), &releases,
				`SELECT`+releaseSelectCols+`FROM update_releases WHERE channel = ? ORDER BY released_at DESC`,
				channel)
		} else {
			err = db.SelectContext(r.Context(), &releases,
				`SELECT`+releaseSelectCols+`FROM update_releases ORDER BY released_at DESC`)
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

// CreateRelease handles POST /v2/updates/releases as multipart/form-data.
// The .exe is uploaded to R2; SHA-256 is computed server-side.
func CreateRelease(db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s3conn == nil {
			jsonError(w, http.StatusServiceUnavailable, "S3 storage is not configured")
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

		var pDescEN, pDescIT, pMinVer *string
		if descEN != "" {
			pDescEN = &descEN
		}
		if descIT != "" {
			pDescIT = &descIT
		}
		if minVer != "" {
			pMinVer = &minVer
		}

		_, err = db.ExecContext(r.Context(),
			`INSERT INTO update_releases
			 (version, channel, download_filename, sha256_checksum, short_note, severity_type,
			  description_en, description_it, is_critical, min_required_version, released_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			version, channel, filename, checksum, shortNote,
			severityType, pDescEN, pDescIT, isCritical, pMinVer, releasedAt,
		)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to create release: "+err.Error())
			return
		}

		jsonCreated(w, map[string]string{
			"version":         version,
			"channel":         channel,
			"download_filename": filename,
			"sha256_checksum": checksum,
		})
	}
}

func DownloadRelease(db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s3conn == nil {
			jsonError(w, http.StatusServiceUnavailable, "S3 storage is not configured")
			return
		}

		version := chi.URLParam(r, "version")

		var filename string
		if err := db.GetContext(r.Context(), &filename,
			`SELECT download_filename FROM update_releases WHERE version = ?`, version); err != nil {
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
		version := chi.URLParam(r, "version")

		var filename string
		err := db.GetContext(r.Context(), &filename,
			`SELECT download_filename FROM update_releases WHERE version = ?`, version)
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
			`DELETE FROM update_releases WHERE version = ?`, version)
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

func PatchReleaseChannel(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		// Archive whoever currently holds the target channel slot
		if req.Channel != "archived" {
			if _, err = tx.ExecContext(r.Context(),
				`UPDATE update_releases SET channel = 'archived' WHERE channel = ? AND version != ?`,
				req.Channel, version,
			); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to archive existing release")
				return
			}
		}

		res, err := tx.ExecContext(r.Context(),
			`UPDATE update_releases SET channel = ? WHERE version = ?`, req.Channel, version)
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

		jsonOK(w, map[string]string{"version": version, "channel": req.Channel})
	}
}

func buildManifest(releases []models.Release, apiBaseURL string) models.UpdateManifest {
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

		switch rel.Channel {
		case "stable":
			m.StableVersion = rel.Version
			m.StableDownload = fmt.Sprintf("%s/v2/updates/releases/%s/download", apiBaseURL, rel.Version)
			m.IsCritical = rel.IsCritical
			if rel.MinRequiredVersion != nil {
				m.MinRequiredVersion = *rel.MinRequiredVersion
			}
		case "beta":
			m.BetaVersion = rel.Version
			m.BetaDownload = fmt.Sprintf("%s/v2/updates/releases/%s/download", apiBaseURL, rel.Version)
		}
	}

	if len(m.DetailedReleaseNotes) == 0 {
		m.DetailedReleaseNotes = nil
	}

	return m
}
