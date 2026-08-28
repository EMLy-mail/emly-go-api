package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
	"emly-api-go/internal/storage"
	"emly-api-go/internal/timing"
)

// updaterVersionPattern enforces the manifest contract: semver with no "v"
// prefix, so the version the client compares against its own version.Version
// after restarting matches the installer's ApplicationVersion exactly.
var updaterVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}(-[0-9A-Za-z.\-]+)?$`)

const updaterReleaseSelectCols = `
	id, version, is_current, download_filename, sha256_checksum, file_size,
	notes_it, notes_en, published_at, created_at `

// clearCurrentUpdaterFlag demotes whichever release currently holds the
// is_current slot. Unlike EMLy releases there is only one slot and no
// channels: the updater takes whatever it is served.
func clearCurrentUpdaterFlag(ctx context.Context, tx *sqlx.Tx, exceptVersion string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE updater_releases SET is_current = 0 WHERE is_current = 1 AND version != ?`, exceptVersion)
	return err
}

// updaterInstallerFilename sanitizes an uploaded filename down to a bare base
// name so it can never escape the configured S3 prefix, falling back to the
// name the CI publishes.
func updaterInstallerFilename(uploaded, version string) string {
	name := path.Base(strings.ReplaceAll(strings.TrimSpace(uploaded), `\`, "/"))
	if name == "" || name == "." || name == "/" {
		return "EMLyUpdater_Installer_" + version + ".exe"
	}
	return name
}

// GetUpdaterManifest handles GET /v2/updates/manifest/updater - the EMLy
// Updater's own self-update manifest.
//
// It answers 200 in every non-error case, including "nothing to distribute",
// which serializes as {"version": ""} and the client treats as a silent no-op.
// 404 is deliberately never returned here: the client reads a 404 as "this
// mirror does not implement the endpoint yet" and stops without retrying, so
// using it for an empty catalogue would be indistinguishable from an
// out-of-date mirror.
func GetUpdaterManifest(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var rel models.UpdaterRelease
		err := db.GetContext(r.Context(), &rel,
			`SELECT`+updaterReleaseSelectCols+`FROM updater_releases WHERE is_current = 1 LIMIT 1`)
		timing.Mark(r.Context(), "db_select")

		if errors.Is(err, sql.ErrNoRows) {
			jsonOK(w, models.UpdaterManifest{})
			return
		}
		if err != nil {
			// 5xx makes the client retry with backoff on its next cycle.
			jsonError(w, http.StatusInternalServerError, "failed to fetch updater release")
			return
		}

		manifest := models.UpdaterManifest{
			Version: rel.Version,
			// requestBaseURL honors X-Forwarded-Proto/Host, so an internal
			// site mirror serves a link pointing at itself with no per-site
			// configuration.
			Download:    fmt.Sprintf("%s/v2/updates/download/updater/%s", requestBaseURL(r), rel.Version),
			SHA256:      rel.SHA256Checksum,
			Size:        rel.FileSize,
			PublishedAt: rel.PublishedAt.UTC().Format(time.RFC3339),
		}

		notes := make(map[string]string, 2)
		if rel.NotesIT != nil && *rel.NotesIT != "" {
			notes["it"] = *rel.NotesIT
		}
		if rel.NotesEN != nil && *rel.NotesEN != "" {
			notes["en"] = *rel.NotesEN
		}
		if len(notes) > 0 {
			manifest.ReleaseNotes = notes
		}

		timing.Mark(r.Context(), "build_manifest")
		jsonOK(w, manifest)

		uaVersion, _ := parseUpdaterUserAgent(r.UserAgent())
		recordUpdaterEvent(r.Context(), db, r, "manifest_check", productUpdater, uaVersion)
	}
}

// DownloadUpdater handles GET /v2/updates/download/updater/{version}, serving
// the signed installer byte for byte. It stays unauthenticated like the EMLy
// release download: the manifest's download link may legitimately be fetched
// through a mirror or CDN that does not forward the API key.
func DownloadUpdater(db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s3conn == nil {
			jsonError(w, http.StatusServiceUnavailable, "S3 storage is not configured")
			return
		}

		version := chi.URLParam(r, "version")

		var filename string
		if err := db.GetContext(r.Context(), &filename,
			`SELECT download_filename FROM updater_releases WHERE version = ?`, version); err != nil {
			jsonError(w, http.StatusNotFound, "updater release not found")
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

		recordUpdaterEvent(r.Context(), db, r, "download", productUpdater, version)
	}
}

// ListUpdaterReleases handles GET /v2/updates/updater/releases.
func ListUpdaterReleases(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var releases []models.UpdaterRelease
		if err := db.SelectContext(r.Context(), &releases,
			`SELECT`+updaterReleaseSelectCols+`FROM updater_releases ORDER BY published_at DESC`,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch updater releases")
			return
		}
		jsonOK(w, releases)
	}
}

// CreateUpdaterRelease handles POST /v2/updates/updater/releases as
// multipart/form-data. The signed installer is uploaded to the updates bucket
// under the updater prefix and SHA-256 is computed server-side, so the
// checksum in the manifest always describes the bytes actually served.
func CreateUpdaterRelease(db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string) http.HandlerFunc {
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
		if version == "" {
			jsonError(w, http.StatusBadRequest, "version is required")
			return
		}
		if !updaterVersionPattern.MatchString(version) {
			jsonError(w, http.StatusBadRequest, "version must be semver without a leading 'v' (e.g. 1.5.0)")
			return
		}

		isCurrent := r.FormValue("is_current") == "true" || r.FormValue("is_current") == "1"
		notesIT := strings.TrimSpace(r.FormValue("notes_it"))
		notesEN := strings.TrimSpace(r.FormValue("notes_en"))

		publishedAt := time.Now().UTC()
		if s := strings.TrimSpace(r.FormValue("published_at")); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				jsonError(w, http.StatusBadRequest, "published_at must be RFC3339")
				return
			}
			publishedAt = t.UTC()
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
		filename := updaterInstallerFilename(header.Filename, version)

		if _, err := s3conn.UploadFile(r.Context(), s3Key(s3Prefix, filename),
			bytes.NewReader(data), "application/octet-stream", nil); err != nil {
			jsonError(w, http.StatusInternalServerError, "upload failed: "+err.Error())
			return
		}

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback() //nolint:errcheck

		if isCurrent {
			if err := clearCurrentUpdaterFlag(r.Context(), tx, version); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing current release")
				return
			}
		}

		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO updater_releases
			 (version, is_current, download_filename, sha256_checksum, file_size, notes_it, notes_en, published_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			version, isCurrent, filename, checksum, int64(len(data)),
			nullableString(notesIT), nullableString(notesEN), publishedAt,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to create updater release: "+err.Error())
			return
		}

		if err := tx.Commit(); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		jsonCreated(w, map[string]interface{}{
			"version":           version,
			"is_current":        isCurrent,
			"download_filename": filename,
			"sha256_checksum":   checksum,
			"file_size":         len(data),
		})
	}
}

type patchUpdaterReleaseRequest struct {
	IsCurrent   *bool   `json:"is_current"`
	NotesIT     *string `json:"notes_it"`
	NotesEN     *string `json:"notes_en"`
	PublishedAt *string `json:"published_at"`
}

// PatchUpdaterRelease handles PATCH /v2/updates/updater/releases/{version}.
// {"is_current": false} on the released build is the kill-switch: the manifest
// immediately falls back to {"version": ""} and the fleet stops picking it up.
// There is no downgrade path - to roll back, publish a higher version carrying
// the previous code.
func PatchUpdaterRelease(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version := chi.URLParam(r, "version")

		var req patchUpdaterReleaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		var setClauses []string
		var args []interface{}

		if req.IsCurrent != nil {
			setClauses = append(setClauses, "is_current = ?")
			args = append(args, *req.IsCurrent)
		}
		if req.NotesIT != nil {
			setClauses = append(setClauses, "notes_it = ?")
			args = append(args, nullableString(strings.TrimSpace(*req.NotesIT)))
		}
		if req.NotesEN != nil {
			setClauses = append(setClauses, "notes_en = ?")
			args = append(args, nullableString(strings.TrimSpace(*req.NotesEN)))
		}
		if req.PublishedAt != nil {
			t, err := time.Parse(time.RFC3339, *req.PublishedAt)
			if err != nil {
				jsonError(w, http.StatusBadRequest, "published_at must be RFC3339")
				return
			}
			setClauses = append(setClauses, "published_at = ?")
			args = append(args, t.UTC())
		}

		if len(setClauses) == 0 {
			jsonError(w, http.StatusBadRequest, "no fields to update")
			return
		}
		args = append(args, version)

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback() //nolint:errcheck

		if req.IsCurrent != nil && *req.IsCurrent {
			if err := clearCurrentUpdaterFlag(r.Context(), tx, version); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing current release")
				return
			}
		}

		res, err := tx.ExecContext(r.Context(),
			"UPDATE updater_releases SET "+strings.Join(setClauses, ", ")+" WHERE version = ?", args...)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to update updater release")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			jsonError(w, http.StatusNotFound, "updater release not found")
			return
		}

		if err := tx.Commit(); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		var updated models.UpdaterRelease
		if err := db.GetContext(r.Context(), &updated,
			`SELECT`+updaterReleaseSelectCols+`FROM updater_releases WHERE version = ?`, version,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch updated updater release")
			return
		}
		jsonOK(w, updated)
	}
}

// DeleteUpdaterRelease handles DELETE /v2/updates/updater/releases/{version},
// removing both the stored installer and its row.
func DeleteUpdaterRelease(db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s3conn == nil {
			jsonError(w, http.StatusServiceUnavailable, "S3 storage is not configured")
			return
		}

		version := chi.URLParam(r, "version")

		var filename string
		if err := db.GetContext(r.Context(), &filename,
			`SELECT download_filename FROM updater_releases WHERE version = ?`, version); err != nil {
			jsonError(w, http.StatusNotFound, "updater release not found")
			return
		}

		if err := s3conn.DeleteFile(r.Context(), s3Key(s3Prefix, filename)); err != nil && !storage.IsNotFound(err) {
			jsonError(w, http.StatusInternalServerError, "failed to delete file from storage: "+err.Error())
			return
		}

		res, err := db.ExecContext(r.Context(),
			`DELETE FROM updater_releases WHERE version = ?`, version)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to delete updater release: "+err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			jsonError(w, http.StatusNotFound, "updater release not found")
			return
		}

		jsonOK(w, map[string]bool{"deleted": true})
	}
}
