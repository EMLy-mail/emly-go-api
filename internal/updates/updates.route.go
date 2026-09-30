package updates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
	"emly-api-go/internal/productreg"
	"emly-api-go/internal/response"
	"emly-api-go/internal/statshub"
	"emly-api-go/internal/storage"
	"emly-api-go/internal/timing"
	"emly-api-go/internal/updaterclient"
)

var validSeverity = map[string]bool{"none": true, "security": true, "bugfix": true, "feature": true}

const releaseSelectCols = `
	id, product, version, is_stable, is_beta, download_filename, sha256_checksum, short_note,
	severity_type, description_en, description_it, is_critical, critical_version, min_required_version,
	released_at, created_at `

// clearStableFlag/clearBetaFlag enforce that at most one release of a product
// holds each channel slot at a time - promoting a release to stable (or beta)
// demotes whoever previously held that slot in the same product, and never
// touches another product's. The two flags are independent, so the same
// release can hold both is_stable and is_beta simultaneously.
func clearStableFlag(ctx context.Context, tx *sqlx.Tx, product, exceptVersion string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE update_releases SET is_stable = 0 WHERE product = ? AND is_stable = 1 AND version != ?`, product, exceptVersion)
	return err
}

func clearBetaFlag(ctx context.Context, tx *sqlx.Tx, product, exceptVersion string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE update_releases SET is_beta = 0 WHERE product = ? AND is_beta = 1 AND version != ?`, product, exceptVersion)
	return err
}

// clearCriticalFlag is the same rule for is_critical: at most one release per
// product carries it.
func clearCriticalFlag(ctx context.Context, tx *sqlx.Tx, product, exceptVersion string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE update_releases SET is_critical = 0, critical_version = NULL WHERE product = ? AND is_critical = 1 AND version != ?`,
		product, exceptVersion)
	return err
}

// requestBaseURL derives the externally-visible scheme+host for the current
// request, so download links in the manifest match whatever hostname/IP the
// client actually used to reach the API, instead of a hardcoded config value.
// Honors X-Forwarded-Proto/X-Forwarded-Host when the API sits behind a proxy.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}

	host := r.Host
	if fwHost := r.Header.Get("X-Forwarded-Host"); fwHost != "" {
		host = fwHost
	}

	return scheme + "://" + host
}

func GetUpdateManifest(db *sqlx.DB, hub *statshub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.DebugContext(r.Context(), "manifest request",
			"method", r.Method,
			"url", r.URL.String(),
			"host", r.Host,
			"remote_addr", r.RemoteAddr,
			"headers", r.Header,
		)

		product := productreg.FromContext(r.Context())

		var releases []models.Release
		err := db.SelectContext(r.Context(), &releases,
			`SELECT`+releaseSelectCols+`FROM update_releases WHERE product = ? ORDER BY released_at DESC`, product.Slug)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch releases")
			return
		}
		timing.Mark(r.Context(), "db_select")
		manifest := buildManifest(releases, requestBaseURL(r), product.Slug)
		timing.Mark(r.Context(), "build_manifest")
		response.OK(w, manifest)

		uaVersion, _ := updaterclient.ParseUserAgent(r.UserAgent())
		updaterclient.RecordEvent(r.Context(), db, r, hub, "manifest_check", product.Slug, uaVersion)
	}
}

func ListReleases(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := r.URL.Query().Get("channel")
		product := productreg.FromContext(r.Context()).Slug

		var filter string
		switch channel {
		case "":
		case "stable":
			filter = ` AND is_stable = 1`
		case "beta":
			filter = ` AND is_beta = 1`
		case "archived":
			filter = ` AND is_stable = 0 AND is_beta = 0`
		default:
			response.Error(w, http.StatusBadRequest, "channel must be one of: stable, beta, archived")
			return
		}

		releases := []models.Release{}
		err := db.SelectContext(r.Context(), &releases,
			`SELECT`+releaseSelectCols+`FROM update_releases WHERE product = ?`+filter+` ORDER BY released_at DESC`, product)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch releases")
			return
		}
		response.OK(w, releases)
	}
}

func s3Key(prefix, filename string) string {
	if prefix == "" {
		return filename
	}
	return prefix + "/" + filename
}

// CreateRelease handles POST /v2/updates/releases (EMLy) and
// POST /v2/updates/{product}/releases as multipart/form-data. The .exe is
// uploaded to the product's folder of the updates S3 bucket (see
// productreg.S3Prefix; s3Prefix is S3_UPDATES_PREFIX); SHA-256 is computed
// server-side.
func CreateRelease(db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		product := productreg.FromContext(r.Context())
		if s3conn == nil {
			response.Error(w, http.StatusServiceUnavailable, "S3 storage is not configured")
			return
		}

		if err := r.ParseMultipartForm(32 << 20); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
			return
		}

		version := strings.TrimSpace(r.FormValue("version"))
		isStable := r.FormValue("is_stable") == "true" || r.FormValue("is_stable") == "1"
		isBeta := r.FormValue("is_beta") == "true" || r.FormValue("is_beta") == "1"
		shortNote := r.FormValue("short_note")
		severityType := strings.TrimSpace(r.FormValue("severity_type"))
		descEN := strings.TrimSpace(r.FormValue("description_en"))
		descIT := strings.TrimSpace(r.FormValue("description_it"))
		isCritical := r.FormValue("is_critical") == "true" || r.FormValue("is_critical") == "1"
		criticalVer := strings.TrimSpace(r.FormValue("critical_version"))
		minVer := strings.TrimSpace(r.FormValue("min_required_version"))
		releasedAtStr := strings.TrimSpace(r.FormValue("released_at"))

		if version == "" {
			response.Error(w, http.StatusBadRequest, "version is required")
			return
		}
		if severityType == "" {
			severityType = "none"
		}
		if !validSeverity[severityType] {
			response.Error(w, http.StatusBadRequest, "severity_type must be one of: none, security, bugfix, feature")
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
			response.Error(w, http.StatusBadRequest, "missing file field")
			return
		}
		defer file.Close()

		data, err := io.ReadAll(file)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to read file: "+err.Error())
			return
		}

		sum := sha256.Sum256(data)
		checksum := hex.EncodeToString(sum[:])
		filename := header.Filename

		if _, err := s3conn.UploadFile(r.Context(), s3Key(productreg.S3Prefix(product, s3Prefix), filename), bytes.NewReader(data), "application/octet-stream", nil); err != nil {
			response.Error(w, http.StatusInternalServerError, "upload failed: "+err.Error())
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
			response.Error(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback()

		if isCritical {
			if err = clearCriticalFlag(r.Context(), tx, product.Slug, version); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to clear existing critical flag")
				return
			}
		}

		if isStable {
			if err = clearStableFlag(r.Context(), tx, product.Slug, version); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to clear existing stable release")
				return
			}
		}
		if isBeta {
			if err = clearBetaFlag(r.Context(), tx, product.Slug, version); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to clear existing beta release")
				return
			}
		}

		_, err = tx.ExecContext(r.Context(),
			`INSERT INTO update_releases
			 (product, version, is_stable, is_beta, download_filename, sha256_checksum, short_note, severity_type,
			  description_en, description_it, is_critical, critical_version, min_required_version, released_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			product.Slug, version, isStable, isBeta, filename, checksum, shortNote,
			severityType, pDescEN, pDescIT, isCritical, pCriticalVer, pMinVer, releasedAt,
		)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to create release: "+err.Error())
			return
		}

		if err := tx.Commit(); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		response.Created(w, map[string]interface{}{
			"product":           product.Slug,
			"version":           version,
			"is_stable":         isStable,
			"is_beta":           isBeta,
			"download_filename": filename,
			"sha256_checksum":   checksum,
		})
	}
}

func DownloadRelease(db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string, hub *statshub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s3conn == nil {
			response.Error(w, http.StatusServiceUnavailable, "S3 storage is not configured")
			return
		}

		version := chi.URLParam(r, "version")
		product := productreg.FromContext(r.Context())

		var filename string
		if err := db.GetContext(r.Context(), &filename,
			`SELECT download_filename FROM update_releases WHERE product = ? AND version = ?`, product.Slug, version); err != nil {
			response.Error(w, http.StatusNotFound, "release not found")
			return
		}

		rc, info, err := s3conn.GetFile(r.Context(), s3Key(productreg.S3Prefix(product, s3Prefix), filename))
		if err != nil {
			if storage.IsNotFound(err) {
				response.Error(w, http.StatusNotFound, "installer file not found in storage")
				return
			}
			response.Error(w, http.StatusInternalServerError, "failed to retrieve file: "+err.Error())
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

		streamInstaller(w, r, rc, product.Slug, version, filename, info.Size)

		// See the identical comment in DownloadUpdater: the client closing the
		// connection right after the last byte races r.Context()'s
		// cancellation against this bookkeeping write, so it must not depend
		// on that context living past the response.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		updaterclient.RecordEvent(ctx, db, r, hub, "download", product.Slug, version)
	}
}

func DeleteRelease(db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s3conn == nil {
			response.Error(w, http.StatusServiceUnavailable, "S3 storage is not configured")
			return
		}

		version := chi.URLParam(r, "version")
		product := productreg.FromContext(r.Context())

		var filename string
		err := db.GetContext(r.Context(), &filename,
			`SELECT download_filename FROM update_releases WHERE product = ? AND version = ?`, product.Slug, version)
		if err != nil {
			response.Error(w, http.StatusNotFound, "release not found")
			return
		}

		if err := s3conn.DeleteFile(r.Context(), s3Key(productreg.S3Prefix(product, s3Prefix), filename)); err != nil && !storage.IsNotFound(err) {
			response.Error(w, http.StatusInternalServerError, "failed to delete file from storage: "+err.Error())
			return
		}

		res, err := db.ExecContext(r.Context(),
			`DELETE FROM update_releases WHERE product = ? AND version = ?`, product.Slug, version)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to delete release: "+err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			response.Error(w, http.StatusNotFound, "release not found")
			return
		}

		response.OK(w, map[string]bool{"deleted": true})
	}
}

type patchReleaseChannelsRequest struct {
	IsStable *bool `json:"is_stable"`
	IsBeta   *bool `json:"is_beta"`
}

type putReleaseRequest struct {
	IsStable           bool    `json:"is_stable"`
	IsBeta             bool    `json:"is_beta"`
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
	IsStable           *bool   `json:"is_stable"`
	IsBeta             *bool   `json:"is_beta"`
	ShortNote          *string `json:"short_note"`
	SeverityType       *string `json:"severity_type"`
	DescriptionEN      *string `json:"description_en"`
	DescriptionIT      *string `json:"description_it"`
	IsCritical         *bool   `json:"is_critical"`
	CriticalVersion    *string `json:"critical_version"`
	MinRequiredVersion *string `json:"min_required_version"`
	ReleasedAt         *string `json:"released_at"`
}

// PatchReleaseChannels handles PATCH /v2/updates/releases/{version}/channel.
// is_stable and is_beta are independent flags: setting either to true
// demotes whoever currently holds that slot, but a single release may hold
// both at once (e.g. it is simultaneously the current stable and beta build).
func PatchReleaseChannels(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version := chi.URLParam(r, "version")
		product := productreg.FromContext(r.Context()).Slug

		var req patchReleaseChannelsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if req.IsStable == nil && req.IsBeta == nil {
			response.Error(w, http.StatusBadRequest, "is_stable and/or is_beta required")
			return
		}

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback()

		var setClauses []string
		var args []interface{}

		if req.IsStable != nil {
			if *req.IsStable {
				if err = clearStableFlag(r.Context(), tx, product, version); err != nil {
					response.Error(w, http.StatusInternalServerError, "failed to clear existing stable release")
					return
				}
			}
			setClauses = append(setClauses, "is_stable = ?")
			args = append(args, *req.IsStable)
		}
		if req.IsBeta != nil {
			if *req.IsBeta {
				if err = clearBetaFlag(r.Context(), tx, product, version); err != nil {
					response.Error(w, http.StatusInternalServerError, "failed to clear existing beta release")
					return
				}
			}
			setClauses = append(setClauses, "is_beta = ?")
			args = append(args, *req.IsBeta)
		}
		args = append(args, product, version)

		res, err := tx.ExecContext(r.Context(),
			"UPDATE update_releases SET "+strings.Join(setClauses, ", ")+" WHERE product = ? AND version = ?", args...)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to update channels")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			response.Error(w, http.StatusNotFound, "release not found")
			return
		}

		if err := tx.Commit(); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		var updated models.Release
		if err := db.GetContext(r.Context(), &updated,
			`SELECT`+releaseSelectCols+`FROM update_releases WHERE product = ? AND version = ?`, product, version,
		); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch updated release")
			return
		}
		response.OK(w, map[string]interface{}{
			"version":   updated.Version,
			"is_stable": updated.IsStable,
			"is_beta":   updated.IsBeta,
		})
	}
}

func PutRelease(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version := chi.URLParam(r, "version")
		product := productreg.FromContext(r.Context()).Slug

		var req putReleaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		if req.SeverityType == "" {
			req.SeverityType = "none"
		}
		if !validSeverity[req.SeverityType] {
			response.Error(w, http.StatusBadRequest, "severity_type must be one of: none, security, bugfix, feature")
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
			response.Error(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback()

		if req.IsStable {
			if err = clearStableFlag(r.Context(), tx, product, version); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to clear existing stable release")
				return
			}
		}
		if req.IsBeta {
			if err = clearBetaFlag(r.Context(), tx, product, version); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to clear existing beta release")
				return
			}
		}

		if req.IsCritical {
			if err = clearCriticalFlag(r.Context(), tx, product, version); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to clear existing critical flag")
				return
			}
		}

		res, err := tx.ExecContext(r.Context(),
			`UPDATE update_releases
			 SET is_stable = ?, is_beta = ?, short_note = ?, severity_type = ?,
			     description_en = ?, description_it = ?, is_critical = ?, critical_version = ?,
			     min_required_version = ?, released_at = ?
			 WHERE product = ? AND version = ?`,
			req.IsStable, req.IsBeta, req.ShortNote, req.SeverityType,
			req.DescriptionEN, req.DescriptionIT, req.IsCritical, req.CriticalVersion,
			req.MinRequiredVersion, releasedAt, product, version,
		)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to update release")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			response.Error(w, http.StatusNotFound, "release not found")
			return
		}

		if err := tx.Commit(); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		var updated models.Release
		if err := db.GetContext(r.Context(), &updated,
			`SELECT`+releaseSelectCols+`FROM update_releases WHERE product = ? AND version = ?`, product, version,
		); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch updated release")
			return
		}
		response.OK(w, updated)
	}
}

func PatchRelease(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version := chi.URLParam(r, "version")
		product := productreg.FromContext(r.Context()).Slug

		var req patchReleaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		if req.SeverityType != nil && !validSeverity[*req.SeverityType] {
			response.Error(w, http.StatusBadRequest, "severity_type must be one of: none, security, bugfix, feature")
			return
		}

		var setClauses []string
		var args []interface{}

		if req.IsStable != nil {
			setClauses = append(setClauses, "is_stable = ?")
			args = append(args, *req.IsStable)
		}
		if req.IsBeta != nil {
			setClauses = append(setClauses, "is_beta = ?")
			args = append(args, *req.IsBeta)
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
				response.Error(w, http.StatusBadRequest, "released_at must be RFC3339")
				return
			}
			setClauses = append(setClauses, "released_at = ?")
			args = append(args, t.UTC())
		}

		if len(setClauses) == 0 {
			response.Error(w, http.StatusBadRequest, "no fields to update")
			return
		}

		args = append(args, product, version)

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback()

		if req.IsStable != nil && *req.IsStable {
			if err = clearStableFlag(r.Context(), tx, product, version); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to clear existing stable release")
				return
			}
		}
		if req.IsBeta != nil && *req.IsBeta {
			if err = clearBetaFlag(r.Context(), tx, product, version); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to clear existing beta release")
				return
			}
		}

		if req.IsCritical != nil && *req.IsCritical {
			if err = clearCriticalFlag(r.Context(), tx, product, version); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to clear existing critical flag")
				return
			}
		}

		query := "UPDATE update_releases SET " + strings.Join(setClauses, ", ") + " WHERE product = ? AND version = ?"
		res, err := tx.ExecContext(r.Context(), query, args...)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to update release")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			response.Error(w, http.StatusNotFound, "release not found")
			return
		}

		if err := tx.Commit(); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		var updated models.Release
		if err := db.GetContext(r.Context(), &updated,
			`SELECT`+releaseSelectCols+`FROM update_releases WHERE product = ? AND version = ?`, product, version,
		); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch updated release")
			return
		}
		response.OK(w, updated)
	}
}

// releaseDownloadURL is the link a manifest advertises for one release. EMLy
// keeps the product-less route every EMLy client already in the field was
// given; every other product gets its own /v2/updates/{product}/... link.
func releaseDownloadURL(apiBaseURL, product, version string) string {
	if product == productreg.EMLy {
		return fmt.Sprintf("%s/v2/updates/releases/%s/download", apiBaseURL, version)
	}
	return fmt.Sprintf("%s/v2/updates/%s/releases/%s/download", apiBaseURL, product, version)
}

func buildManifest(releases []models.Release, apiBaseURL, product string) models.UpdateManifest {
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

		// is_stable and is_beta are independent, so the same release can
		// populate both the stable and beta slots of the manifest at once.
		if rel.IsStable {
			m.StableVersion = rel.Version
			m.StableDownload = releaseDownloadURL(apiBaseURL, product, rel.Version)
			if rel.MinRequiredVersion != nil {
				m.MinRequiredVersion = *rel.MinRequiredVersion
			}
		}
		if rel.IsBeta {
			m.BetaVersion = rel.Version
			m.BetaDownload = releaseDownloadURL(apiBaseURL, product, rel.Version)
		}
	}

	if len(m.DetailedReleaseNotes) == 0 {
		m.DetailedReleaseNotes = nil
	}

	return m
}
