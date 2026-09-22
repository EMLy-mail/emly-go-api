// Package eventprune keeps updater_events bounded. The table takes one row
// per manifest check per client per cycle, which on a few-hundred-machine
// fleet is tens of thousands of rows a day and no natural ceiling - it had
// reached half a million rows with nothing ever deleting from it.
//
// What makes deleting safe is that nothing aggregate reads these rows any
// more: /v2/stats/summary and /v2/stats/events are served from the
// updater_event_hourly rollup (migration 20), which is written at ingest and
// kept forever. Raw rows are only read for one thing, the recent per-event
// history on GET /v2/stats/clients/{id}, so the retention window is a choice
// about how far back that detail goes - the charts and totals are unaffected
// and keep their full history.
//
// The one thing lost with the rows is migration 20's repair statement, which
// rebuilds the rollup by re-aggregating raw events: past the retention window
// there is nothing left to re-aggregate. That is why the counter is
// incremented inside the ingest transaction (see insertUpdaterEvent) rather
// than by a job that could silently fall behind.
//
// Like internal/configmirror this is a background loop owned by main.go, and
// like internal/logfile's own retention it only ever deletes what it can
// identify as in scope.
package eventprune

import (
	"context"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	// Interval is how often the loop looks for expired rows. Retention is
	// measured in days, so there is nothing to gain from checking more often
	// than hourly, and hourly keeps each run's batch small enough to be
	// invisible next to ordinary traffic.
	Interval = time.Hour

	// batchSize bounds one DELETE. A single statement covering a whole
	// backlog - the first run after this ships has to clear everything older
	// than the window at once - would hold row locks and grow the undo log
	// for as long as it ran, on a table the ingest path is writing to
	// continuously. Batching turns that into many short transactions that
	// interleave with inserts instead of blocking them.
	batchSize = 5000

	// batchPause yields between batches, so a large backlog is cleared over
	// minutes rather than starving the client-facing ingest path of I/O for
	// however long a tight loop would take.
	batchPause = 200 * time.Millisecond
)

// Start runs the prune loop until ctx is done, pruning rows whose created_at
// is older than retention. A non-positive retention disables pruning
// entirely and Start returns immediately, the same escape hatch
// LOG_RETENTION_DAYS=0 gives the log files.
//
// The first pass runs immediately rather than after the first tick: a restart
// is the moment an operator has just changed the setting and wants to see it
// take effect.
func Start(ctx context.Context, db *sqlx.DB, retention time.Duration) {
	if retention <= 0 {
		slog.Info("event prune: disabled", "retention", retention)
		return
	}

	slog.Info("event prune: started", "retention", retention, "interval", Interval)

	t := time.NewTicker(Interval)
	defer t.Stop()

	for {
		if _, err := Run(ctx, db, retention); err != nil {
			// A failed prune is not an outage: the rows simply stay another
			// hour. Logged at warn so a persistent failure is visible before
			// the table grows back.
			slog.WarnContext(ctx, "event prune: failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Run performs one prune pass and reports how many rows it removed.
//
// It deletes by primary key, never by created_at: no index starts with
// created_at (tasks 13 and 19 removed redundant indexes precisely because
// every extra one is a write amplified on this insert-heavy table), so a
// created_at predicate in a batch loop would full-scan once per batch.
// Instead one query locates the boundary id and every batch is a primary-key
// range delete, which is also the cheapest physical order for InnoDB to
// remove rows in.
func Run(ctx context.Context, db *sqlx.DB, retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention)

	// The oldest row still inside the window, found by walking the primary
	// key from the oldest id: ids are handed out in insertion order, so the
	// scan stops at the first row to keep and only ever reads rows that are
	// about to be deleted anyway - a handful per hourly run once the backlog
	// is gone.
	//
	// A concurrent pair of inserts can in principle put a marginally older
	// created_at behind a higher id, which at the edge means keeping or
	// dropping one row a few milliseconds either side of a boundary measured
	// in days. Not worth a stricter query.
	var firstKeptID int64
	err := db.GetContext(ctx, &firstKeptID,
		`SELECT COALESCE(
		     (SELECT id FROM updater_events WHERE created_at >= ? ORDER BY id ASC LIMIT 1),
		     (SELECT COALESCE(MAX(id), 0) + 1 FROM updater_events)
		 )`, cutoff)
	if err != nil {
		return 0, err
	}
	if firstKeptID <= 1 {
		return 0, nil
	}

	var total int64
	for {
		res, err := db.ExecContext(ctx,
			`DELETE FROM updater_events WHERE id < ? LIMIT ?`, firstKeptID, batchSize)
		if err != nil {
			// Whatever earlier batches committed stays deleted; report it
			// alongside the error so a partial pass is legible in the log.
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < batchSize {
			break
		}

		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(batchPause):
		}
	}

	if total > 0 {
		slog.InfoContext(ctx, "event prune: removed expired events",
			"rows", total, "older_than", cutoff.Format(time.RFC3339), "retention", retention)
	}
	return total, nil
}
