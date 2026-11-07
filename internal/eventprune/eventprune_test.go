package eventprune

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestStartDisabledByNonPositiveRetention pins the EVENTS_RETENTION_DAYS=0
// escape hatch: with pruning off, Start must return without ever reaching the
// database. Passing a nil *sqlx.DB is how that is asserted - any query at all
// would panic here.
func TestStartDisabledByNonPositiveRetention(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		Start(context.Background(), nil, Options{Retention: 0})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return with pruning disabled")
	}
}

// TestStartDisabledByNegativeRetention covers the same guard for a retention
// that came out negative, which a misconfigured EVENTS_RETENTION_DAYS can
// produce. It must read as "off", never as "everything is expired".
func TestStartDisabledByNegativeRetention(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		Start(context.Background(), nil, Options{Retention: -24 * time.Hour})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return with a negative retention")
	}
}

// TestStartArchiveWithoutStoreDeletesNothing pins the safety rule of
// EVENTS_ARCHIVE_ENABLED: asked to archive with no bucket to archive to,
// Start must give up before touching the database (nil DB would panic)
// rather than fall back to deleting unarchived rows.
func TestStartArchiveWithoutStoreDeletesNothing(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		Start(context.Background(), nil, Options{Retention: 24 * time.Hour, Archive: true})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return with archiving required and no store")
	}
}

func TestSQLLiteral(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "NULL"},
		{int64(42), "42"},
		{uint64(7), "7"},
		{true, "1"},
		{[]byte("manifest_check"), "'manifest_check'"},
		{"it's \"q\"\\\n\x00\x1a", `'it\'s \"q\"\\\n\0\Z'`},
		{[]byte{0xff, 0x00}, "X'ff00'"},
		{time.Date(2026, 11, 1, 13, 4, 5, 0, time.FixedZone("CET", 3600)), "'2026-11-01 12:04:05'"},
	}
	for _, c := range cases {
		if got := sqlLiteral(c.in); got != c.want {
			t.Errorf("sqlLiteral(%#v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestWriteDumpSplitsInserts(t *testing.T) {
	cols := []string{"id", "event_type", "version", "created_at"}
	var data [][]any
	for i := 1; i <= rowsPerInsert+1; i++ {
		data = append(data, []any{int64(i), []byte("download"), nil, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	}
	var buf strings.Builder
	if err := writeDump(&buf, "updater_events", cols, data, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "SET time_zone = '+00:00';") {
		t.Error("dump does not pin the session time zone")
	}
	if n := strings.Count(out, "INSERT IGNORE INTO `updater_events` (`id`, `event_type`, `version`, `created_at`) VALUES"); n != 2 {
		t.Errorf("dump has %d INSERT statements, want 2 for %d rows", n, len(data))
	}
	if !strings.Contains(out, "(1, 'download', NULL, '2026-10-01 00:00:00')") {
		t.Error("first row not rendered as expected")
	}
	if !strings.HasSuffix(out, fmt.Sprintf("(%d, 'download', NULL, '2026-10-01 00:00:00');\n", rowsPerInsert+1)) {
		t.Error("dump does not end with the last row and a semicolon")
	}
}

func TestArchiveKey(t *testing.T) {
	day := time.Date(2026, 10, 2, 23, 0, 0, 0, time.UTC)
	cases := []struct{ table, prefix, want string }{
		{TableUpdaterEvents, "", DefaultArchivePrefix + "/2026/10/02/updater_events_1-5000.sql.gz"},
		{TableUpdaterEvents, "backups/events", "backups/events/2026/10/02/updater_events_1-5000.sql.gz"},
		{TableUpdaterEvents, "/backups/events/", "backups/events/2026/10/02/updater_events_1-5000.sql.gz"},
		{TableClientWSEvents, "", "emly-api-files/archive/updater_client_ws_events/2026/10/02/updater_client_ws_events_1-5000.sql.gz"},
		{TableClientWSEvents, "ws/", "ws/2026/10/02/updater_client_ws_events_1-5000.sql.gz"},
	}
	for _, c := range cases {
		if got := archiveKey(c.table, c.prefix, day, 1, 5000); got != c.want {
			t.Errorf("archiveKey(%q, %q) = %s, want %s", c.table, c.prefix, got, c.want)
		}
	}
}

// TestStartRefusesUnknownTable pins the guard that keeps the table name,
// which is interpolated into SQL, to the two constants: anything else must
// return before reaching the database (nil DB would panic).
func TestStartRefusesUnknownTable(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		Start(context.Background(), nil, Options{Table: "users; DROP TABLE user", Retention: 24 * time.Hour})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return for an unknown table")
	}
}

func TestRunRefusesUnknownTable(t *testing.T) {
	if _, err := Run(context.Background(), nil, "bug_reports", 24*time.Hour, nil, ""); err == nil {
		t.Fatal("Run accepted a table outside the allowlist")
	}
}
