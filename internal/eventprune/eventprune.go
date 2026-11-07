// Package eventprune keeps the event tables bounded: updater_events and
// updater_client_ws_events (Options.Table), one loop each, same rules.
//
// updater_events is the large one. The table takes one row
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
//
// With EVENTS_ARCHIVE_ENABLED, every batch is first dumped as a gzip'd SQL
// script (INSERT IGNORE statements) to the configured bucket and folder
// (Options.Store, Options.Prefix), and only the rows of a dump that uploaded are deleted: the
// detail leaves the database but not the company. See archive.go.
package eventprune

import (
	"context"
	"fmt"
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

// The tables a loop may prune. The name is interpolated into the queries, so
// it can only ever be one of these constants - Start refuses anything else.
const (
	TableUpdaterEvents  = "updater_events"
	TableClientWSEvents = "updater_client_ws_events"
)

var prunable = map[string]bool{TableUpdaterEvents: true, TableClientWSEvents: true}

// Options configures Start. Archive asks for every deleted row to be dumped
// to Store first; Archive with a nil Store suspends pruning altogether rather
// than deleting unarchived rows (the bucket was unconfigured or unreachable at
// startup, and storage connectors are only built then).
type Options struct {
	// Table is the table to prune; empty means TableUpdaterEvents. Both
	// tables share the shape this needs: an auto-increment id handed out in
	// insertion order and a created_at.
	Table     string
	Retention time.Duration
	Archive   bool
	Store     Archiver
	// Prefix is the folder inside Store; empty means DefaultArchivePrefix
	// for updater_events, archive/<table> next to it for any other table.
	Prefix string
}

// Start runs the prune loop until ctx is done, pruning rows whose created_at
// is older than retention. A non-positive retention disables pruning
// entirely and Start returns immediately, the same escape hatch
// LOG_RETENTION_DAYS=0 gives the log files.
//
// The first pass runs immediately rather than after the first tick: a restart
// is the moment an operator has just changed the setting and wants to see it
// take effect.
func Start(ctx context.Context, db *sqlx.DB, opts Options) {
	table := opts.Table
	if table == "" {
		table = TableUpdaterEvents
	}
	if !prunable[table] {
		slog.Error("event prune: refusing unknown table", "table", table)
		return
	}
	retention := opts.Retention
	if retention <= 0 {
		slog.Info("event prune: disabled", "table", table, "retention", retention)
		return
	}
	var store Archiver
	if opts.Archive {
		if opts.Store == nil {
			slog.Error("event prune: suspended, EVENTS_ARCHIVE_ENABLED is set but the archive bucket is not configured or unreachable; no rows will be deleted", "table", table)
			return
		}
		store = opts.Store
	}

	slog.Info("event prune: started", "table", table, "retention", retention, "interval", Interval, "archive", store != nil, "archive_prefix", opts.Prefix)

	t := time.NewTicker(Interval)
	defer t.Stop()

	for {
		if _, err := Run(ctx, db, table, retention, store, opts.Prefix); err != nil {
			// A failed prune is not an outage: the rows simply stay another
			// hour. Logged at warn so a persistent failure is visible before
			// the table grows back.
			slog.WarnContext(ctx, "event prune: failed", "table", table, "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Run performs one prune pass over table (one of the Table* constants) and
// reports how many rows it removed. A
// non-nil store archives each batch under prefix before deleting it
// (archiveBatch).
//
// It deletes by primary key, never by created_at: no index starts with
// created_at (tasks 13 and 19 removed redundant indexes precisely because
// every extra one is a write amplified on this insert-heavy table), so a
// created_at predicate in a batch loop would full-scan once per batch.
// Instead one query locates the boundary id and every batch is a primary-key
// range delete, which is also the cheapest physical order for InnoDB to
// remove rows in.
func Run(ctx context.Context, db *sqlx.DB, table string, retention time.Duration, store Archiver, prefix string) (int64, error) {
	if !prunable[table] {
		return 0, fmt.Errorf("eventprune: unknown table %q", table)
	}
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
	err := db.GetContext(ctx, &firstKeptID, fmt.Sprintf(
		`SELECT COALESCE(
		     (SELECT id FROM %[1]s WHERE created_at >= ? ORDER BY id ASC LIMIT 1),
		     (SELECT COALESCE(MAX(id), 0) + 1 FROM %[1]s)
		 )`, table), cutoff)
	if err != nil {
		return 0, err
	}
	if firstKeptID <= 1 {
		return 0, nil
	}

	var total int64
	for {
		n, err := deleteBatch(ctx, db, table, store, prefix, firstKeptID)
		// Whatever earlier batches committed stays deleted; report it
		// alongside the error so a partial pass is legible in the log.
		total += n
		if err != nil {
			return total, err
		}
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
			"table", table, "rows", total, "older_than", cutoff.Format(time.RFC3339), "retention", retention, "archived", store != nil)
	}
	return total, nil
}

// deleteBatch removes up to batchSize rows below firstKeptID, archiving them
// first when store is set.
func deleteBatch(ctx context.Context, db *sqlx.DB, table string, store Archiver, prefix string, firstKeptID int64) (int64, error) {
	if store != nil {
		return archiveBatch(ctx, db, table, store, prefix, firstKeptID)
	}
	res, err := db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE id < ? LIMIT ?`, table), firstKeptID, batchSize)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
