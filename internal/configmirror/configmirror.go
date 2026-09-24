// Package configmirror implements the site-mirror side of remote
// configuration replication (API design doc §9): when CONFIG_UPSTREAM_URL is
// set, this instance never accepts config writes and instead polls an
// upstream /v2/config on an interval, validating and storing whatever it
// gets exactly as a new published revision - so every mirror and the cloud
// instance hand out the same bytes under the same revision.
package configmirror

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/config"
	"emly-api-go/internal/remoteconfig"
)

// ConfigNotifier is told when a replicated revision is newly published on
// this mirror, so its own connected v2 client channel sessions re-fetch
// GET /v2/config now instead of at their next refresh - the same contract
// configapi.ConfigNotifier documents for a direct publish/rollback. Defined
// separately (rather than importing configapi's interface) because Go
// interfaces are satisfied structurally: main.go passes the same
// *clienthub.Hub to both without either package depending on the other.
type ConfigNotifier interface {
	NotifyConfigPublished(revision int64)
}

// notifyIfNew tells notifier about revision only when isNew is true -
// storeReplicatedRevision's own "already the published row here" no-op
// (a resync of a revision this mirror had already replicated, e.g. after a
// restart) must not re-announce config.published to this site's connected
// clients on every poll. Tolerates a nil notifier, same as
// configapi.notifyPublished.
func notifyIfNew(notifier ConfigNotifier, isNew bool, revision int64) {
	if !isNew || notifier == nil {
		return
	}
	notifier.NotifyConfigPublished(revision)
}

// State tracks the mirror's last sync outcome for GET /v2/health reporting
// (API design doc §9: "config_upstream: { revision, fetched_at, last_error }").
// Safe for concurrent use: written by the sync loop, read by health handlers.
type State struct {
	mu        sync.RWMutex
	revision  int64
	fetchedAt time.Time
	lastError string
}

// Snapshot returns the current state as a JSON-ready map. revision is 0 and
// fetched_at is nil until the first successful sync (the mirror answers 204
// to its own clients until then, same as the design doc's §15 open question
// on mirror bootstrap).
func (s *State) Snapshot() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := map[string]interface{}{"revision": s.revision}
	if s.fetchedAt.IsZero() {
		out["fetched_at"] = nil
	} else {
		out["fetched_at"] = s.fetchedAt.Format(time.RFC3339)
	}
	if s.lastError == "" {
		out["last_error"] = nil
	} else {
		out["last_error"] = s.lastError
	}
	return out
}

func (s *State) recordSuccess(revision int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision = revision
	s.fetchedAt = time.Now().UTC()
	s.lastError = ""
}

func (s *State) recordNoChange() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetchedAt = time.Now().UTC()
	s.lastError = ""
}

func (s *State) recordError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = err.Error()
}

// Start launches the replication loop in a goroutine and returns a State to
// report through /v2/health. When cfg.ConfigUpstreamURL is empty (the cloud/
// primary instance) it returns a zero State and starts nothing - callers can
// unconditionally call Start and pass the result to the health handler.
// The loop stops when ctx is cancelled. notifier may be nil (a build that
// never constructs a clienthub.Hub); notifyIfNew tolerates that the same way
// configapi.notifyPublished does.
func Start(ctx context.Context, db *sqlx.DB, cfg *config.Config, notifier ConfigNotifier) *State {
	state := &State{}
	if cfg.ConfigUpstreamURL == "" {
		return state
	}

	go run(ctx, db, cfg, notifier, state)
	return state
}

func run(ctx context.Context, db *sqlx.DB, cfg *config.Config, notifier ConfigNotifier, state *State) {
	client := &http.Client{Timeout: 15 * time.Second}

	// One attempt right away so a freshly started mirror doesn't wait a
	// full interval before serving anything.
	syncOnce(ctx, db, cfg, client, notifier, state)

	ticker := time.NewTicker(cfg.ConfigUpstreamInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncOnce(ctx, db, cfg, client, notifier, state)
		}
	}
}

func syncOnce(ctx context.Context, db *sqlx.DB, cfg *config.Config, client *http.Client, notifier ConfigNotifier, state *State) {
	localETag, err := currentPublishedETag(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "config mirror: failed to read local state", "err", err)
		state.recordError(err)
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cfg.ConfigUpstreamURL+"/v2/config", nil)
	if err != nil {
		state.recordError(err)
		return
	}
	req.Header.Set("X-API-Key", cfg.ConfigUpstreamAPIKey)
	if localETag != "" {
		req.Header.Set("If-None-Match", `"`+localETag+`"`)
	}

	resp, err := client.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "config mirror: upstream fetch failed", "upstream", cfg.ConfigUpstreamURL, "err", err)
		state.recordError(err)
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified, http.StatusNoContent:
		state.recordNoChange()
		return
	case http.StatusOK:
		// fall through
	default:
		err := fmt.Errorf("upstream returned %d", resp.StatusCode)
		slog.WarnContext(ctx, "config mirror: upstream fetch failed", "upstream", cfg.ConfigUpstreamURL, "err", err)
		state.recordError(err)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, remoteconfig.MaxDocumentBytes+1))
	if err != nil {
		state.recordError(err)
		return
	}

	// A mirror never serves what it could not validate itself, even though
	// upstream (by construction) never publishes an invalid document - a
	// defense against a compromised or misbehaving upstream.
	doc, problems := remoteconfig.Parse(body)
	if len(problems) > 0 {
		err := fmt.Errorf("upstream document failed validation: %s", problems[0].Message)
		slog.ErrorContext(ctx, "config mirror: rejecting invalid upstream document", "upstream", cfg.ConfigUpstreamURL, "problems", problems)
		state.recordError(err)
		return
	}

	upstreamETag := normalizeETag(resp.Header.Get("ETag"))
	_, computedETag := remoteconfig.Canonical(doc)
	if upstreamETag != "" && upstreamETag != computedETag {
		err := fmt.Errorf("hash mismatch: computed %s != upstream ETag %s", computedETag, upstreamETag)
		slog.ErrorContext(ctx, "config mirror: rejecting document, hash mismatch", "upstream", cfg.ConfigUpstreamURL, "err", err)
		state.recordError(err)
		return
	}
	etag := upstreamETag
	if etag == "" {
		etag = computedETag
	}

	generatedAt, err := time.Parse(time.RFC3339, doc.GeneratedAt)
	if err != nil {
		err = fmt.Errorf("invalid generatedAt in upstream document: %w", err)
		state.recordError(err)
		return
	}

	isNew, err := storeReplicatedRevision(ctx, db, doc.Revision, doc.SchemaVersion, body, etag, generatedAt)
	if err != nil {
		slog.ErrorContext(ctx, "config mirror: failed to store replicated revision", "revision", doc.Revision, "err", err)
		state.recordError(err)
		return
	}

	slog.InfoContext(ctx, "config mirror: synced revision", "revision", doc.Revision, "upstream", cfg.ConfigUpstreamURL)
	state.recordSuccess(doc.Revision)
	// Only for a revision this mirror had not already replicated (I5): a
	// mirror was previously silent here even for a genuinely new revision,
	// so this site's own connected v2 client channel sessions never heard
	// config.published and only found out at their next scheduled refresh.
	notifyIfNew(notifier, isNew, doc.Revision)
}

func normalizeETag(header string) string {
	header = strings.TrimSpace(header)
	header = strings.TrimPrefix(header, "W/")
	return strings.Trim(header, `"`)
}

func currentPublishedETag(ctx context.Context, db *sqlx.DB) (string, error) {
	var etag string
	err := db.GetContext(ctx, &etag, `SELECT etag FROM remote_config_revisions WHERE status = 'published' LIMIT 1`)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return etag, err
}

// storeReplicatedRevision inserts the upstream document verbatim (its bytes
// are stored as received, not re-canonicalized - API design doc §9) as a
// published revision, superseding whatever this mirror previously published.
// Idempotent against re-fetching the same revision (e.g. after a mirror
// restart): if it is already the published row here, this is a no-op.
// The returned bool is that idempotency check's negation - true when this
// call actually replicated a revision the mirror had not already stored,
// false for the no-op resync - so the caller (I5) can tell "just synced the
// same thing again" apart from "a genuinely new revision landed" before
// deciding whether to announce config.published to this site's own clients.
func storeReplicatedRevision(ctx context.Context, db *sqlx.DB, revision int64, schemaVersion int, document []byte, etag string, generatedAt time.Time) (isNew bool, err error) {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck

	var alreadyCurrent bool
	if err := tx.GetContext(ctx, &alreadyCurrent,
		`SELECT COUNT(*) > 0 FROM remote_config_revisions WHERE revision = ? AND status = 'published'`, revision,
	); err != nil {
		return false, err
	}
	if alreadyCurrent {
		return false, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE remote_config_revisions SET status = 'superseded' WHERE status = 'published'`,
	); err != nil {
		return false, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO remote_config_revisions
		   (revision, schema_version, status, document, etag, notes, created_by, based_on, generated_at, published_at)
		 VALUES (?, ?, 'published', ?, ?, NULL, NULL, NULL, ?, NOW())
		 ON DUPLICATE KEY UPDATE
		   status = 'published', document = VALUES(document), etag = VALUES(etag),
		   generated_at = VALUES(generated_at), published_at = NOW()`,
		revision, schemaVersion, string(document), etag, generatedAt,
	); err != nil {
		return false, err
	}

	return true, tx.Commit()
}
