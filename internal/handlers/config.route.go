package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/config"
	"emly-api-go/internal/models"
	"emly-api-go/internal/remoteconfig"
	"emly-api-go/internal/timing"
)

const remoteConfigSelectCols = `
	revision, schema_version, status, document, etag, notes, created_by, based_on,
	generated_at, published_at, created_at `

var validRevisionStatus = map[string]bool{"draft": true, "published": true, "superseded": true}

// jsonProblems writes a 4xx response carrying every remoteconfig.Problem
// found, not just the first (API design doc §7.2/§7.7), so a dashboard can
// show them all at once.
func jsonProblems(w http.ResponseWriter, status int, msg string, problems []remoteconfig.Problem) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": msg, "problems": problems})
}

// revisionEchoWarnings reports, as warning-severity Problems (never
// rejecting the document), the two things the canonical round-trip silently
// drops from an operator-submitted document: a revision/generatedAt the
// operator sent (the server owns both, API design doc §7.2 step 1) and any
// field remoteconfig doesn't recognize anywhere in the schema (step 2).
func revisionEchoWarnings(raw []byte) []remoteconfig.Problem {
	var warnings []remoteconfig.Problem

	var probe struct {
		Revision    *json.RawMessage `json:"revision"`
		GeneratedAt *json.RawMessage `json:"generatedAt"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil {
		if probe.Revision != nil {
			warnings = append(warnings, remoteconfig.Problem{Path: "/revision", Message: "revision is assigned by the server; the submitted value was ignored"})
		}
		if probe.GeneratedAt != nil {
			warnings = append(warnings, remoteconfig.Problem{Path: "/generatedAt", Message: "generatedAt is assigned by the server; the submitted value was ignored"})
		}
	}

	if unknown, err := remoteconfig.UnknownFieldPaths(raw); err == nil {
		for _, p := range unknown {
			warnings = append(warnings, remoteconfig.Problem{Path: p, Message: "unknown field, dropped"})
		}
	}

	return warnings
}

// ifNoneMatchHits reports whether the If-None-Match header covers etag (hex,
// unquoted). Handles a weak validator (W/"...") and a comma-separated list,
// since some proxies weaken or merge validators in front of a site mirror.
func ifNoneMatchHits(header, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "W/")
		part = strings.Trim(part, `"`)
		if part == "*" || part == etag {
			return true
		}
	}
	return false
}

// sessionUsername resolves the dashboard user attached to the request's
// X-Session-Token, if any. It is best-effort attribution (API design doc
// §7.2): an admin-key-only request (no session token, or an invalid/expired
// one) publishes anonymously rather than failing the request.
func sessionUsername(r *http.Request, db *sqlx.DB) *string {
	token := r.Header.Get(sessionHeader)
	if token == "" {
		return nil
	}
	var row struct {
		Username  string    `db:"username"`
		ExpiresAt time.Time `db:"expires_at"`
	}
	err := db.GetContext(r.Context(), &row,
		`SELECT u.username, s.expires_at FROM session s JOIN user u ON u.id = s.user_id WHERE s.id = ? LIMIT 1`,
		token,
	)
	if err != nil || time.Now().UTC().After(row.ExpiresAt) {
		return nil
	}
	return &row.Username
}

// trackConfigFetch best-effort records the client sighting and its
// config_revision/config_fetched_at (API design doc §8), reusing the same
// client identity resolution as the updater manifest path. A request
// carrying neither X-EMLy-HWID nor X-EMLy-Hostname is served but not
// tracked, same as recordUpdaterEvent.
func trackConfigFetch(r *http.Request, db *sqlx.DB, revision int64) {
	ctx := r.Context()
	hwid := r.Header.Get("X-EMLy-HWID")
	hostname := r.Header.Get("X-EMLy-Hostname")
	if hwid == "" && hostname == "" {
		return
	}
	adDomain := r.Header.Get("X-EMLy-ADDomain")
	uaVersion, contact := parseUpdaterUserAgent(r.UserAgent())
	ip := clientIPFromRequest(r)

	clientID, err := upsertUpdaterClient(ctx, db, hwid, hostname, adDomain, uaVersion, contact, ip)
	if err != nil {
		slog.WarnContext(ctx, "config fetch: failed to upsert client", "error", err)
		return
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE updater_clients SET config_revision = ?, config_fetched_at = NOW() WHERE id = ?`,
		revision, clientID,
	); err != nil {
		slog.WarnContext(ctx, "config fetch: failed to record client config revision", "error", err)
	}
}

// GetConfig handles GET /v2/config - the public policy document endpoint
// fetched by the EMLy Updater and by EMLy itself (API design doc §5.1).
//
// 204, never 404, when nothing is published: a 404 from a perfectly healthy
// API just means no document has been published yet, and the client treats
// any 4xx as an outage worth logging. 204 says "reachable, nothing to give
// you, keep what you have" - the same reasoning that makes the updater
// manifest answer 200 {"version": ""} instead of 404.
func GetConfig(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var row models.RemoteConfigRevision
		err := db.GetContext(r.Context(), &row,
			`SELECT`+remoteConfigSelectCols+`FROM remote_config_revisions WHERE status = 'published' LIMIT 1`)
		timing.Mark(r.Context(), "db_select")

		w.Header().Set("Cache-Control", "no-cache")

		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch published config")
			return
		}

		w.Header().Set("ETag", `"`+row.ETag+`"`)
		w.Header().Set("X-Config-Revision", strconv.FormatInt(row.Revision, 10))

		if ifNoneMatchHits(r.Header.Get("If-None-Match"), row.ETag) {
			w.WriteHeader(http.StatusNotModified)
			trackConfigFetch(r, db, row.Revision)
			return
		}

		// Written from the stored column with w.Write, not jsonOK:
		// re-encoding would risk changing the bytes the ETag was computed
		// over (API design doc §5.1).
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(row.Document))
		timing.Mark(r.Context(), "write_response")

		trackConfigFetch(r, db, row.Revision)
	}
}

// ValidateConfig handles POST /v2/config/validate (API design doc §7.7).
// Runs the same validation a publish would and stores nothing - for CI
// pipelines and a dashboard's "check" button.
func ValidateConfig(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Document json.RawMessage `json:"document"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if len(req.Document) == 0 {
			jsonError(w, http.StatusBadRequest, "document is required")
			return
		}

		_, problems := remoteconfig.Parse(req.Document)
		if len(problems) > 0 {
			jsonProblems(w, http.StatusUnprocessableEntity, "invalid document", problems)
			return
		}

		jsonOK(w, map[string]interface{}{"valid": true, "warnings": revisionEchoWarnings(req.Document)})
	}
}

// ListConfigRevisions handles GET /v2/config/revisions (API design doc §7.1).
func ListConfigRevisions(db *sqlx.DB) http.HandlerFunc {
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
		offset := (page - 1) * pageSize

		status := r.URL.Query().Get("status")
		whereClause := ""
		var whereArgs []interface{}
		if status != "" {
			if !validRevisionStatus[status] {
				jsonError(w, http.StatusBadRequest, "status must be one of: draft, published, superseded")
				return
			}
			whereClause = " WHERE status = ?"
			whereArgs = append(whereArgs, status)
		}

		var total int
		if err := db.GetContext(r.Context(), &total,
			`SELECT COUNT(*) FROM remote_config_revisions`+whereClause, whereArgs...); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to count revisions")
			return
		}

		listQuery := `SELECT r.revision, r.schema_version, r.status, r.etag, r.notes, r.created_by, r.based_on,
			r.generated_at, r.published_at, r.created_at,
			(SELECT COUNT(*) FROM updater_clients uc WHERE uc.config_revision = r.revision) AS clients_on_revision
			FROM remote_config_revisions r` + whereClause + ` ORDER BY r.revision DESC LIMIT ? OFFSET ?`
		listArgs := append(append([]interface{}{}, whereArgs...), pageSize, offset)

		var revisions []models.RemoteConfigRevisionSummary
		if err := db.SelectContext(r.Context(), &revisions, listQuery, listArgs...); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch revisions")
			return
		}

		jsonOK(w, map[string]interface{}{
			"page":      page,
			"page_size": pageSize,
			"total":     total,
			"revisions": revisions,
		})
	}
}

// GetConfigRevision handles GET /v2/config/revisions/{revision} (§7.6).
func GetConfigRevision(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		revision, err := strconv.ParseInt(chi.URLParam(r, "revision"), 10, 64)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid revision")
			return
		}
		var row models.RemoteConfigRevision
		if err := db.GetContext(r.Context(), &row,
			`SELECT`+remoteConfigSelectCols+`FROM remote_config_revisions WHERE revision = ?`, revision,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				jsonError(w, http.StatusNotFound, "revision not found")
				return
			}
			jsonError(w, http.StatusInternalServerError, "failed to fetch revision")
			return
		}
		jsonOK(w, row)
	}
}

// DeleteConfigRevision handles DELETE /v2/config/revisions/{revision} (§7.6).
// Only a draft can be deleted; it does not free the revision number.
func DeleteConfigRevision(db *sqlx.DB, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.ConfigUpstreamURL != "" {
			jsonError(w, http.StatusMethodNotAllowed, "this instance mirrors "+cfg.ConfigUpstreamURL+"; publish there")
			return
		}
		revision, err := strconv.ParseInt(chi.URLParam(r, "revision"), 10, 64)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid revision")
			return
		}

		res, err := db.ExecContext(r.Context(),
			`DELETE FROM remote_config_revisions WHERE revision = ? AND status = 'draft'`, revision)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to delete revision")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var status string
			if err2 := db.GetContext(r.Context(), &status, `SELECT status FROM remote_config_revisions WHERE revision = ?`, revision); err2 != nil {
				jsonError(w, http.StatusNotFound, "revision not found")
				return
			}
			jsonError(w, http.StatusConflict, "only draft revisions can be deleted")
			return
		}
		jsonOK(w, map[string]bool{"deleted": true})
	}
}

// CreateConfigRevision handles POST /v2/config/revisions (API design doc
// §7.2). document must not carry revision/generatedAt - both are assigned
// here and any submitted value is reported as a warning, never an error.
func CreateConfigRevision(db *sqlx.DB, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.ConfigUpstreamURL != "" {
			jsonError(w, http.StatusMethodNotAllowed, "this instance mirrors "+cfg.ConfigUpstreamURL+"; publish there")
			return
		}

		var req struct {
			Document json.RawMessage `json:"document"`
			Notes    *string         `json:"notes"`
			Publish  bool            `json:"publish"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if len(req.Document) == 0 {
			jsonError(w, http.StatusBadRequest, "document is required")
			return
		}
		if len(req.Document) > remoteconfig.MaxDocumentBytes {
			jsonError(w, http.StatusRequestEntityTooLarge, "document exceeds the 1 MiB size cap")
			return
		}

		doc, problems := remoteconfig.Parse(req.Document)
		if len(problems) > 0 {
			jsonProblems(w, http.StatusUnprocessableEntity, "invalid document", problems)
			return
		}
		warnings := revisionEchoWarnings(req.Document)
		createdBy := sessionUsername(r, db)

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback() //nolint:errcheck

		var maxRev sql.NullInt64
		if err := tx.GetContext(r.Context(), &maxRev, `SELECT MAX(revision) FROM remote_config_revisions FOR UPDATE`); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to allocate revision")
			return
		}
		revision := int64(1)
		if maxRev.Valid {
			revision = maxRev.Int64 + 1
		}

		doc.Revision = revision
		generatedAt := time.Now().UTC()
		doc.GeneratedAt = generatedAt.Format(time.RFC3339)
		body, etag := remoteconfig.Canonical(doc)

		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO remote_config_revisions (revision, schema_version, status, document, etag, notes, created_by, based_on, generated_at)
			 VALUES (?, ?, 'draft', ?, ?, ?, ?, NULL, ?)`,
			revision, doc.SchemaVersion, string(body), etag, notesOrNil(req.Notes), createdBy, generatedAt,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to create revision: "+err.Error())
			return
		}

		status := "draft"
		var publishedAt *time.Time
		if req.Publish {
			if err := publishRevisionTx(r.Context(), tx, revision); err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to publish: "+err.Error())
				return
			}
			status = "published"
			now := time.Now().UTC()
			publishedAt = &now
		}

		if err := tx.Commit(); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		if req.Publish {
			slog.InfoContext(r.Context(), "config revision published", "revision", revision,
				"created_by", createdBy, "client_ip", clientIPFromRequest(r), "notes", req.Notes)
		}

		jsonCreated(w, map[string]interface{}{
			"revision":       revision,
			"schema_version": doc.SchemaVersion,
			"status":         status,
			"document":       doc,
			"etag":           etag,
			"notes":          req.Notes,
			"created_by":     createdBy,
			"based_on":       nil,
			"generated_at":   generatedAt,
			"published_at":   publishedAt,
			"warnings":       warnings,
		})
	}
}

// publishRevisionTx supersedes whatever is currently published and marks
// revision published, inside tx. Returns errRevisionNotDraft when revision
// isn't a draft (0 rows affected on the second update) - the caller decides
// what that means in its own context (already-published vs a stale draft).
func publishRevisionTx(ctx context.Context, tx *sqlx.Tx, revision int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE remote_config_revisions SET status = 'superseded' WHERE status = 'published'`); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE remote_config_revisions SET status = 'published', published_at = NOW() WHERE revision = ? AND status = 'draft'`, revision)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errRevisionNotDraft
	}
	return nil
}

var errRevisionNotDraft = errors.New("revision is not a draft")

func notesOrNil(s *string) *string {
	if s == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// PublishConfigRevision handles POST /v2/config/revisions/{revision}/publish
// (API design doc §7.3).
func PublishConfigRevision(db *sqlx.DB, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.ConfigUpstreamURL != "" {
			jsonError(w, http.StatusMethodNotAllowed, "this instance mirrors "+cfg.ConfigUpstreamURL+"; publish there")
			return
		}
		revision, err := strconv.ParseInt(chi.URLParam(r, "revision"), 10, 64)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid revision")
			return
		}

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback() //nolint:errcheck

		var status string
		if err := tx.GetContext(r.Context(), &status, `SELECT status FROM remote_config_revisions WHERE revision = ? FOR UPDATE`, revision); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				jsonError(w, http.StatusNotFound, "revision not found")
				return
			}
			jsonError(w, http.StatusInternalServerError, "failed to fetch revision")
			return
		}
		switch status {
		case "published":
			jsonError(w, http.StatusConflict, "already published")
			return
		case "superseded":
			jsonError(w, http.StatusConflict, "superseded; use rollback to republish its content")
			return
		}

		var currentPublished sql.NullInt64
		if err := tx.GetContext(r.Context(), &currentPublished,
			`SELECT revision FROM remote_config_revisions WHERE status = 'published' LIMIT 1`); err != nil && !errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusInternalServerError, "failed to check current published revision")
			return
		}
		if currentPublished.Valid && revision <= currentPublished.Int64 {
			jsonError(w, http.StatusConflict, "a newer revision has been published since; create a new draft")
			return
		}

		if err := publishRevisionTx(r.Context(), tx, revision); err != nil {
			jsonError(w, http.StatusConflict, err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		slog.InfoContext(r.Context(), "config revision published", "revision", revision,
			"created_by", sessionUsername(r, db), "client_ip", clientIPFromRequest(r))

		var row models.RemoteConfigRevision
		if err := db.GetContext(r.Context(), &row, `SELECT`+remoteConfigSelectCols+`FROM remote_config_revisions WHERE revision = ?`, revision); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch published revision")
			return
		}
		jsonOK(w, row)
	}
}

// RollbackConfig handles POST /v2/config/rollback (API design doc §7.4).
// The only rollback mechanism: clones an old revision's content into a new,
// higher-numbered one and publishes it, so a lagging mirror can never see a
// lower revision than what the fleet already has.
func RollbackConfig(db *sqlx.DB, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.ConfigUpstreamURL != "" {
			jsonError(w, http.StatusMethodNotAllowed, "this instance mirrors "+cfg.ConfigUpstreamURL+"; publish there")
			return
		}

		var req struct {
			To    int64   `json:"to"`
			Notes *string `json:"notes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if req.To <= 0 {
			jsonError(w, http.StatusBadRequest, "to is required")
			return
		}

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback() //nolint:errcheck

		var source models.RemoteConfigRevision
		if err := tx.GetContext(r.Context(), &source,
			`SELECT`+remoteConfigSelectCols+`FROM remote_config_revisions WHERE revision = ? AND status != 'draft'`, req.To,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				jsonError(w, http.StatusNotFound, "source revision not found (or is still a draft)")
				return
			}
			jsonError(w, http.StatusInternalServerError, "failed to fetch source revision")
			return
		}

		doc, problems := remoteconfig.Parse([]byte(source.Document))
		if len(problems) > 0 {
			jsonProblems(w, http.StatusUnprocessableEntity, "source revision no longer validates under the current rules", problems)
			return
		}

		var maxRev sql.NullInt64
		if err := tx.GetContext(r.Context(), &maxRev, `SELECT MAX(revision) FROM remote_config_revisions FOR UPDATE`); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to allocate revision")
			return
		}
		newRevision := int64(1)
		if maxRev.Valid {
			newRevision = maxRev.Int64 + 1
		}

		doc.Revision = newRevision
		generatedAt := time.Now().UTC()
		doc.GeneratedAt = generatedAt.Format(time.RFC3339)
		body, etag := remoteconfig.Canonical(doc)
		createdBy := sessionUsername(r, db)

		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO remote_config_revisions (revision, schema_version, status, document, etag, notes, created_by, based_on, generated_at)
			 VALUES (?, ?, 'draft', ?, ?, ?, ?, ?, ?)`,
			newRevision, doc.SchemaVersion, string(body), etag, notesOrNil(req.Notes), createdBy, req.To, generatedAt,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to create rollback revision: "+err.Error())
			return
		}

		if err := publishRevisionTx(r.Context(), tx, newRevision); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to publish rollback: "+err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		slog.InfoContext(r.Context(), "config rolled back", "revision", newRevision, "based_on", req.To,
			"created_by", createdBy, "client_ip", clientIPFromRequest(r), "notes", req.Notes)

		var row models.RemoteConfigRevision
		if err := db.GetContext(r.Context(), &row, `SELECT`+remoteConfigSelectCols+`FROM remote_config_revisions WHERE revision = ?`, newRevision); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch rolled-back revision")
			return
		}
		jsonCreated(w, row)
	}
}

// PreviewConfig handles POST /v2/config/preview (API design doc §7.5).
// Exactly one of revision (stored) or document (inline, validated first,
// never stored) is required.
func PreviewConfig(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Revision *int64           `json:"revision"`
			Document *json.RawMessage `json:"document"`
			Host     struct {
				HWID     string   `json:"hwid"`
				Hostname string   `json:"hostname"`
				DC       string   `json:"dc"`
				IPs      []string `json:"ips"`
				Domain   string   `json:"domain"`
				Now      *string  `json:"now"`
			} `json:"host"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		hasRevision := req.Revision != nil
		hasDocument := req.Document != nil && len(*req.Document) > 0
		if hasRevision == hasDocument {
			jsonError(w, http.StatusBadRequest, "exactly one of revision or document is required")
			return
		}

		var doc *remoteconfig.Document
		if hasDocument {
			parsed, problems := remoteconfig.Parse(*req.Document)
			if len(problems) > 0 {
				jsonProblems(w, http.StatusUnprocessableEntity, "invalid document", problems)
				return
			}
			doc = parsed
		} else {
			var stored string
			if err := db.GetContext(r.Context(), &stored, `SELECT document FROM remote_config_revisions WHERE revision = ?`, *req.Revision); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					jsonError(w, http.StatusNotFound, "revision not found")
					return
				}
				jsonError(w, http.StatusInternalServerError, "failed to fetch revision")
				return
			}
			parsed, problems := remoteconfig.Parse([]byte(stored))
			if len(problems) > 0 {
				jsonError(w, http.StatusInternalServerError, "stored revision failed to re-validate")
				return
			}
			doc = parsed
		}

		host := remoteconfig.Host{
			HWID:     req.Host.HWID,
			Hostname: req.Host.Hostname,
			DC:       req.Host.DC,
			Domain:   req.Host.Domain,
			IPs:      req.Host.IPs,
		}
		if req.Host.Now != nil && *req.Host.Now != "" {
			t, err := time.Parse(time.RFC3339, *req.Host.Now)
			if err != nil {
				jsonError(w, http.StatusBadRequest, "host.now must be RFC3339")
				return
			}
			host.Now = t
		}

		effective, appliedIDs := remoteconfig.Effective(doc, host)
		matchedSite, chain := remoteconfig.ResolveSite(effective, host)
		if appliedIDs == nil {
			appliedIDs = []string{}
		}

		jsonOK(w, map[string]interface{}{
			"revision":             doc.Revision,
			"effective_document":   effective,
			"applied_override_ids": appliedIDs,
			"matched_site":         matchedSite,
			"resolver_chain":       chain,
		})
	}
}
