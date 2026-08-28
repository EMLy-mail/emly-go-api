package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
	"emly-api-go/internal/storage"
	"emly-api-go/internal/timing"
)

var validSeverity = map[string]bool{"none": true, "security": true, "bugfix": true, "feature": true}

// Values for updater_events.product: which piece of software an event is
// about. The EMLy Updater checks for EMLy releases and, separately, for its
// own new builds - both from the same machine and the same headers.
const (
	productEMLy    = "emly"
	productUpdater = "updater"
)

// updaterUAPattern matches the EMLy Updater's User-Agent, e.g.
// "EMLy-Updater/1.3.0 (f.fois@3git.eu)".
var updaterUAPattern = regexp.MustCompile(`^EMLy-Updater/([\w.\-]+)\s*\(([^)]*)\)`)

func parseUpdaterUserAgent(ua string) (version, contact string) {
	m := updaterUAPattern.FindStringSubmatch(ua)
	if m == nil {
		return "", ""
	}
	return m[1], strings.TrimSpace(m[2])
}

func clientIPFromRequest(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// recordUpdaterEvent best-effort persists an EMLy Updater client sighting and
// operation event. It never fails the caller's HTTP response - a client
// missing the identifying header is silently skipped, and DB errors are only
// logged, since this is telemetry on a client-facing path.
//
// product is productEMLy for traffic about the EMLy app and productUpdater for
// the updater's own self-update, so the two never get mixed in fleet stats.
func recordUpdaterEvent(ctx context.Context, db *sqlx.DB, r *http.Request, eventType, product, version string) {
	hostname := r.Header.Get("X-EMLy-Hostname")
	if hostname == "" {
		return
	}
	adDomain := r.Header.Get("X-EMLy-ADDomain")
	uaVersion, contact := parseUpdaterUserAgent(r.UserAgent())
	ip := clientIPFromRequest(r)

	res, err := db.ExecContext(ctx,
		`INSERT INTO updater_clients (hostname, ad_domain, updater_version, contact, last_ip)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		     updater_version = VALUES(updater_version),
		     contact = VALUES(contact),
		     last_ip = VALUES(last_ip),
		     last_seen_at = CURRENT_TIMESTAMP,
		     id = LAST_INSERT_ID(id)`,
		hostname, adDomain, nullableString(uaVersion), nullableString(contact), nullableString(ip),
	)
	if err != nil {
		slog.WarnContext(ctx, "updater stats: failed to upsert client", "error", err)
		return
	}

	clientID, err := res.LastInsertId()
	if err != nil {
		slog.WarnContext(ctx, "updater stats: failed to resolve client id", "error", err)
		return
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO updater_events (client_id, event_type, product, version, ip_address) VALUES (?, ?, ?, ?, ?)`,
		clientID, eventType, product, nullableString(version), nullableString(ip),
	); err != nil {
		slog.WarnContext(ctx, "updater stats: failed to record event", "error", err)
	}
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

const releaseSelectCols = `
	id, version, is_stable, is_beta, download_filename, sha256_checksum, short_note,
	severity_type, description_en, description_it, is_critical, critical_version, min_required_version,
	released_at, created_at `

// clearStableFlag/clearBetaFlag enforce that at most one release holds each
// channel slot at a time - promoting a release to stable (or beta) demotes
// whoever previously held that slot. The two flags are independent, so the
// same release can hold both is_stable and is_beta simultaneously.
func clearStableFlag(ctx context.Context, tx *sqlx.Tx, exceptVersion string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE update_releases SET is_stable = 0 WHERE is_stable = 1 AND version != ?`, exceptVersion)
	return err
}

func clearBetaFlag(ctx context.Context, tx *sqlx.Tx, exceptVersion string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE update_releases SET is_beta = 0 WHERE is_beta = 1 AND version != ?`, exceptVersion)
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

func GetUpdateManifest(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.DebugContext(r.Context(), "manifest request",
			"method", r.Method,
			"url", r.URL.String(),
			"host", r.Host,
			"remote_addr", r.RemoteAddr,
			"headers", r.Header,
		)

		var releases []models.Release
		err := db.SelectContext(r.Context(), &releases,
			`SELECT`+releaseSelectCols+`FROM update_releases ORDER BY released_at DESC`)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch releases")
			return
		}
		timing.Mark(r.Context(), "db_select")
		manifest := buildManifest(releases, requestBaseURL(r))
		timing.Mark(r.Context(), "build_manifest")
		jsonOK(w, manifest)

		uaVersion, _ := parseUpdaterUserAgent(r.UserAgent())
		recordUpdaterEvent(r.Context(), db, r, "manifest_check", productEMLy, uaVersion)
	}
}

func ListReleases(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := r.URL.Query().Get("channel")

		var releases []models.Release
		var err error
		switch channel {
		case "":
			err = db.SelectContext(r.Context(), &releases,
				`SELECT`+releaseSelectCols+`FROM update_releases ORDER BY released_at DESC`)
		case "stable":
			err = db.SelectContext(r.Context(), &releases,
				`SELECT`+releaseSelectCols+`FROM update_releases WHERE is_stable = 1 ORDER BY released_at DESC`)
		case "beta":
			err = db.SelectContext(r.Context(), &releases,
				`SELECT`+releaseSelectCols+`FROM update_releases WHERE is_beta = 1 ORDER BY released_at DESC`)
		case "archived":
			err = db.SelectContext(r.Context(), &releases,
				`SELECT`+releaseSelectCols+`FROM update_releases WHERE is_stable = 0 AND is_beta = 0 ORDER BY released_at DESC`)
		default:
			jsonError(w, http.StatusBadRequest, "channel must be one of: stable, beta, archived")
			return
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
// The .exe is uploaded to the updates S3 bucket; SHA-256 is computed server-side.
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
			jsonError(w, http.StatusBadRequest, "version is required")
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
				`UPDATE update_releases SET is_critical = 0, critical_version = NULL WHERE is_critical = 1`,
			); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing critical flag")
				return
			}
		}

		if isStable {
			if err = clearStableFlag(r.Context(), tx, version); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing stable release")
				return
			}
		}
		if isBeta {
			if err = clearBetaFlag(r.Context(), tx, version); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing beta release")
				return
			}
		}

		_, err = tx.ExecContext(r.Context(),
			`INSERT INTO update_releases
			 (version, is_stable, is_beta, download_filename, sha256_checksum, short_note, severity_type,
			  description_en, description_it, is_critical, critical_version, min_required_version, released_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			version, isStable, isBeta, filename, checksum, shortNote,
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

		jsonCreated(w, map[string]interface{}{
			"version":           version,
			"is_stable":         isStable,
			"is_beta":           isBeta,
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

		recordUpdaterEvent(r.Context(), db, r, "download", productEMLy, version)
	}
}

func DeleteRelease(db *sqlx.DB, s3conn *storage.S3Connector, s3Prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s3conn == nil {
			jsonError(w, http.StatusServiceUnavailable, "S3 storage is not configured")
			return
		}

		version := chi.URLParam(r, "version")

		var filename string
		err := db.GetContext(r.Context(), &filename,
			`SELECT download_filename FROM update_releases WHERE version = ?`, version)
		if err != nil {
			jsonError(w, http.StatusNotFound, "release not found")
			return
		}

		if err := s3conn.DeleteFile(r.Context(), s3Key(s3Prefix, filename)); err != nil && !storage.IsNotFound(err) {
			jsonError(w, http.StatusInternalServerError, "failed to delete file from storage: "+err.Error())
			return
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

		var req patchReleaseChannelsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if req.IsStable == nil && req.IsBeta == nil {
			jsonError(w, http.StatusBadRequest, "is_stable and/or is_beta required")
			return
		}

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback()

		var setClauses []string
		var args []interface{}

		if req.IsStable != nil {
			if *req.IsStable {
				if err = clearStableFlag(r.Context(), tx, version); err != nil {
					jsonError(w, http.StatusInternalServerError, "failed to clear existing stable release")
					return
				}
			}
			setClauses = append(setClauses, "is_stable = ?")
			args = append(args, *req.IsStable)
		}
		if req.IsBeta != nil {
			if *req.IsBeta {
				if err = clearBetaFlag(r.Context(), tx, version); err != nil {
					jsonError(w, http.StatusInternalServerError, "failed to clear existing beta release")
					return
				}
			}
			setClauses = append(setClauses, "is_beta = ?")
			args = append(args, *req.IsBeta)
		}
		args = append(args, version)

		res, err := tx.ExecContext(r.Context(),
			"UPDATE update_releases SET "+strings.Join(setClauses, ", ")+" WHERE version = ?", args...)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to update channels")
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
			`SELECT`+releaseSelectCols+`FROM update_releases WHERE version = ?`, version,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch updated release")
			return
		}
		jsonOK(w, map[string]interface{}{
			"version":   updated.Version,
			"is_stable": updated.IsStable,
			"is_beta":   updated.IsBeta,
		})
	}
}

func PutRelease(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version := chi.URLParam(r, "version")

		var req putReleaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON")
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

		if req.IsStable {
			if err = clearStableFlag(r.Context(), tx, version); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing stable release")
				return
			}
		}
		if req.IsBeta {
			if err = clearBetaFlag(r.Context(), tx, version); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing beta release")
				return
			}
		}

		if req.IsCritical {
			if _, err = tx.ExecContext(r.Context(),
				`UPDATE update_releases SET is_critical = 0, critical_version = NULL WHERE is_critical = 1 AND version != ?`,
				version,
			); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing critical flag")
				return
			}
		}

		res, err := tx.ExecContext(r.Context(),
			`UPDATE update_releases
			 SET is_stable = ?, is_beta = ?, short_note = ?, severity_type = ?,
			     description_en = ?, description_it = ?, is_critical = ?, critical_version = ?,
			     min_required_version = ?, released_at = ?
			 WHERE version = ?`,
			req.IsStable, req.IsBeta, req.ShortNote, req.SeverityType,
			req.DescriptionEN, req.DescriptionIT, req.IsCritical, req.CriticalVersion,
			req.MinRequiredVersion, releasedAt, version,
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
			`SELECT`+releaseSelectCols+`FROM update_releases WHERE version = ?`, version,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch updated release")
			return
		}
		jsonOK(w, updated)
	}
}

func PatchRelease(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version := chi.URLParam(r, "version")

		var req patchReleaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		if req.SeverityType != nil && !validSeverity[*req.SeverityType] {
			jsonError(w, http.StatusBadRequest, "severity_type must be one of: none, security, bugfix, feature")
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

		args = append(args, version)

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback()

		if req.IsStable != nil && *req.IsStable {
			if err = clearStableFlag(r.Context(), tx, version); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing stable release")
				return
			}
		}
		if req.IsBeta != nil && *req.IsBeta {
			if err = clearBetaFlag(r.Context(), tx, version); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing beta release")
				return
			}
		}

		if req.IsCritical != nil && *req.IsCritical {
			if _, err = tx.ExecContext(r.Context(),
				`UPDATE update_releases SET is_critical = 0, critical_version = NULL WHERE is_critical = 1 AND version != ?`,
				version,
			); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to clear existing critical flag")
				return
			}
		}

		query := "UPDATE update_releases SET " + strings.Join(setClauses, ", ") + " WHERE version = ?"
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
			`SELECT`+releaseSelectCols+`FROM update_releases WHERE version = ?`, version,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch updated release")
			return
		}
		jsonOK(w, updated)
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
			m.StableDownload = fmt.Sprintf("%s/v2/updates/releases/%s/download", apiBaseURL, rel.Version)
			if rel.MinRequiredVersion != nil {
				m.MinRequiredVersion = *rel.MinRequiredVersion
			}
		}
		if rel.IsBeta {
			m.BetaVersion = rel.Version
			m.BetaDownload = fmt.Sprintf("%s/v2/updates/releases/%s/download", apiBaseURL, rel.Version)
		}
	}

	if len(m.DetailedReleaseNotes) == 0 {
		m.DetailedReleaseNotes = nil
	}

	return m
}
