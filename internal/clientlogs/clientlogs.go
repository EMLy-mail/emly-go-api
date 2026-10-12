// Package clientlogs keeps the log files machines send back through the
// client channel's log.send_latest command (CLIENT_WS_PROTOCOL.md §7.7), so
// they outlive the in-memory command record internal/clienthub keeps for 24h
// and an API restart.
//
// A result reaches it through clienthub.Hub.OnResult, already off the hub's
// lock and on its own goroutine. The content comes from a machine and is not
// trusted: it is decoded and size-checked before anything is stored, and it
// is only ever served back as an attachment, never as something a browser
// could render.
//
// The gzip is stored as received, in the API file bucket when there is one
// (USE_S3_API_FILE_STORAGE), in the row itself otherwise - the same
// degradation bug report files have. One row per command, keyed by the
// command id, so a log is found from the command that fetched it.
package clientlogs

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/clienthub"
	"emly-api-go/internal/clientproto"
	"emly-api-go/internal/dbvalue"
	"emly-api-go/internal/storage"
)

const (
	// MaxDecompressedBytes bounds what a stored log may inflate to. The
	// updater reads at most the last 2 MiB of its file, and one frame holds
	// about 500 KB of real log: anything past 4 MiB is a gzip bomb, not a log.
	MaxDecompressedBytes = 4 << 20
	// MaxPerClient and Retention bound what is kept: the newest MaxPerClient
	// logs of each machine, none older than Retention.
	MaxPerClient = 20
	Retention    = 30 * 24 * time.Hour
	// PruneInterval is how often Start applies them.
	PruneInterval = time.Hour

	s3Prefix    = "emly-api-files/client-logs"
	saveTimeout = 30 * time.Second
)

// blobs is the part of storage.S3Connector this package uses, so tests can
// stand in for the bucket.
type blobs interface {
	UploadFile(ctx context.Context, key string, body io.Reader, contentType string, metadata map[string]string) (string, error)
	GetFile(ctx context.Context, key string) (io.ReadCloser, *storage.FileInfo, error)
	DeleteFiles(ctx context.Context, keys []string) error
}

// Store saves, lists, serves and prunes client logs. It holds no state of
// its own beyond its dependencies, so building more than one over the same
// db/bucket is harmless.
type Store struct {
	db    *sqlx.DB
	blobs blobs
	now   func() time.Time
}

// New returns a Store. s3 may be nil: logs then live in the database.
func New(db *sqlx.DB, s3 *storage.S3Connector) *Store {
	s := &Store{db: db, now: time.Now}
	if s3 != nil {
		s.blobs = s3 // never a typed nil in the interface
	}
	return s
}

// Record is one stored log, as listed and returned by the admin routes.
type Record struct {
	ID             string     `db:"id" json:"id"`
	ClientID       int64      `db:"client_id" json:"client_id"`
	IssuedBy       *string    `db:"issued_by" json:"issued_by,omitempty"`
	FileName       string     `db:"file_name" json:"file_name"`
	SizeBytes      int64      `db:"size_bytes" json:"size_bytes"`
	OffsetBytes    int64      `db:"offset_bytes" json:"offset"`
	IncludedBytes  int64      `db:"included_bytes" json:"included_bytes"`
	Truncated      bool       `db:"truncated" json:"truncated"`
	Mismatch       bool       `db:"mismatch" json:"mismatch"`
	LogLevel       *string    `db:"log_level" json:"level,omitempty"`
	ModifiedAt     *time.Time `db:"modified_at" json:"modified_at,omitempty"`
	CollectedAt    *time.Time `db:"collected_at" json:"collected_at,omitempty"`
	ReceivedAt     time.Time  `db:"received_at" json:"received_at"`
	GzipBytes      int64      `db:"gzip_bytes" json:"gzip_bytes"`
	UpdaterVersion *string    `db:"updater_version" json:"updater_version,omitempty"`
	Hostname       *string    `db:"hostname" json:"hostname,omitempty"`
}

const recordColumns = `id, client_id, issued_by, file_name, size_bytes, offset_bytes, included_bytes,
	truncated, mismatch, log_level, modified_at, collected_at, received_at, gzip_bytes,
	updater_version, hostname`

// decoded is a log.send_latest payload that passed decodeLogFile.
type decoded struct {
	file clientproto.LogFile
	gz   []byte
	// mismatch: the content inflates to a length other than included_bytes.
	mismatch bool
}

// decodeLogFile parses and checks a log.send_latest result payload: the
// encoding, the base64, the gzip itself, and how large it inflates.
func decodeLogFile(payload json.RawMessage) (decoded, error) {
	var d decoded
	if err := json.Unmarshal(payload, &d.file); err != nil {
		return d, fmt.Errorf("payload is not a LogFile: %w", err)
	}
	if d.file.Encoding != clientproto.LogEncodingGzipBase64 {
		return d, fmt.Errorf("unsupported encoding %q", d.file.Encoding)
	}
	gz, err := base64.StdEncoding.DecodeString(d.file.Content)
	if err != nil {
		return d, fmt.Errorf("content is not base64: %w", err)
	}
	n, err := inflatedSize(gz)
	if err != nil {
		return d, err
	}
	d.gz, d.mismatch = gz, n != d.file.IncludedBytes
	d.file.Content = "" // the bytes live in d.gz from here on
	return d, nil
}

// inflatedSize gunzips gz into nothing, refusing past MaxDecompressedBytes.
func inflatedSize(gz []byte) (int64, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return 0, fmt.Errorf("content is not gzip: %w", err)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(zr, MaxDecompressedBytes+1))
	if err != nil {
		return 0, fmt.Errorf("content is not valid gzip: %w", err)
	}
	if n > MaxDecompressedBytes {
		return 0, fmt.Errorf("content inflates past %d bytes", MaxDecompressedBytes)
	}
	return n, nil
}

// parseClientTime reads an RFC 3339 time the client sent; nil if absent or
// malformed - it is informational, never a reason to drop the log.
func parseClientTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// HandleResult is the clienthub.Hub.OnResult hook: it saves rec when it is a
// finished log.send_latest and ignores every other command.
func (s *Store) HandleResult(rec clienthub.CommandRecord) {
	if rec.Name != clientproto.CmdLogSendLatest || rec.Status != clienthub.StatusDone || rec.Result == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), saveTimeout)
	defer cancel()
	if err := s.Save(ctx, rec.ID, rec.ClientID, rec.IssuedBy, rec.Result.Payload); err != nil {
		slog.WarnContext(ctx, "client log not saved", "command_id", rec.ID, "client_id", rec.ClientID, "err", err)
	}
}

// Save validates and stores one log.send_latest payload under commandID.
// Saving the same command twice keeps the first.
func (s *Store) Save(ctx context.Context, commandID string, clientID int64, issuedBy string, payload json.RawMessage) error {
	if s == nil || s.db == nil {
		return errors.New("no database")
	}
	d, err := decodeLogFile(payload)
	if err != nil {
		return err
	}
	var client struct {
		Hostname       *string `db:"hostname"`
		UpdaterVersion *string `db:"updater_version"`
	}
	if err := s.db.GetContext(ctx, &client,
		`SELECT hostname, updater_version FROM updater_clients WHERE id = ?`, clientID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading the client: %w", err)
	}

	var s3Key *string
	var content []byte
	if s.blobs != nil {
		key := fmt.Sprintf("%s/%d/%s.log.gz", s3Prefix, clientID, commandID)
		if _, err := s.blobs.UploadFile(ctx, key, bytes.NewReader(d.gz), "application/gzip",
			map[string]string{"client-id": fmt.Sprint(clientID), "command-id": commandID}); err != nil {
			// Same degradation as bug report files: an unreachable bucket
			// costs where the bytes live, not the log.
			slog.WarnContext(ctx, "client log upload failed, keeping it in the database", "key", key, "err", err)
			content = d.gz
		} else {
			s3Key = &key
		}
	} else {
		content = d.gz
	}

	_, err = s.db.ExecContext(ctx, `INSERT IGNORE INTO updater_client_logs
		(id, client_id, issued_by, file_name, size_bytes, offset_bytes, included_bytes, truncated, mismatch,
		 log_level, modified_at, collected_at, received_at, gzip_bytes, updater_version, hostname, s3_key, content)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		commandID, clientID, dbvalue.NullString(dbvalue.Truncate(issuedBy, 255)),
		dbvalue.Truncate(d.file.Name, 64), d.file.SizeBytes, d.file.Offset, d.file.IncludedBytes,
		d.file.Offset > 0, d.mismatch, dbvalue.NullString(dbvalue.Truncate(d.file.Level, 10)),
		parseClientTime(d.file.ModifiedAt), parseClientTime(d.file.CollectedAt), s.now().UTC(),
		len(d.gz), client.UpdaterVersion, client.Hostname, s3Key, content)
	if err != nil {
		return fmt.Errorf("inserting the log: %w", err)
	}
	slog.InfoContext(ctx, "client log saved", "command_id", commandID, "client_id", clientID,
		"included_bytes", d.file.IncludedBytes, "truncated", d.file.Offset > 0, "s3", s3Key != nil)
	return nil
}

// List returns a machine's logs, newest first.
func (s *Store) List(ctx context.Context, clientID int64) ([]Record, error) {
	out := []Record{}
	err := s.db.SelectContext(ctx, &out,
		`SELECT `+recordColumns+` FROM updater_client_logs WHERE client_id = ? ORDER BY received_at DESC, id DESC`, clientID)
	return out, err
}

// ErrNotFound is a log id with no row.
var ErrNotFound = errors.New("client log not found")

// Get returns one log's metadata.
func (s *Store) Get(ctx context.Context, id string) (Record, error) {
	var rec Record
	err := s.db.GetContext(ctx, &rec, `SELECT `+recordColumns+` FROM updater_client_logs WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, ErrNotFound
	}
	return rec, err
}

// Gzip returns one log's metadata and its gzip bytes.
func (s *Store) Gzip(ctx context.Context, id string) (Record, []byte, error) {
	var row struct {
		Record
		S3Key   *string `db:"s3_key"`
		Content []byte  `db:"content"`
	}
	err := s.db.GetContext(ctx, &row, `SELECT `+recordColumns+`, s3_key, content FROM updater_client_logs WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return row.Record, nil, ErrNotFound
	}
	if err != nil {
		return row.Record, nil, err
	}
	if row.S3Key == nil {
		return row.Record, row.Content, nil
	}
	if s.blobs == nil {
		return row.Record, nil, errBucketUnavailable
	}
	body, _, err := s.blobs.GetFile(ctx, *row.S3Key)
	if err != nil {
		return row.Record, nil, fmt.Errorf("%w: %v", errBucketUnavailable, err)
	}
	defer body.Close()
	// Bounded like everything else read back: the object is what Save wrote,
	// at most one frame's worth, but the bucket is not this package's alone.
	gz, err := io.ReadAll(io.LimitReader(body, MaxDecompressedBytes))
	if err != nil {
		return row.Record, nil, fmt.Errorf("%w: %v", errBucketUnavailable, err)
	}
	return row.Record, gz, nil
}

var errBucketUnavailable = errors.New("log storage unavailable")

// Delete removes one log, bucket object first: a row without its object is
// a 502 on download, an object without its row is invisible forever.
func (s *Store) Delete(ctx context.Context, id string) error {
	var key *string
	err := s.db.GetContext(ctx, &key, `SELECT s3_key FROM updater_client_logs WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := s.deleteObjects(ctx, []*string{key}); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM updater_client_logs WHERE id = ?`, id)
	return err
}

// deleteObjects removes the bucket objects behind keys (nil entries are
// rows stored in the database). Without a bucket configured any more the
// objects cannot be reached: they are left behind, and the rows go anyway.
func (s *Store) deleteObjects(ctx context.Context, keys []*string) error {
	var list []string
	for _, k := range keys {
		if k != nil {
			list = append(list, *k)
		}
	}
	if len(list) == 0 {
		return nil
	}
	if s.blobs == nil {
		slog.WarnContext(ctx, "client log objects left in the bucket: API file S3 storage is not configured", "count", len(list))
		return nil
	}
	return s.blobs.DeleteFiles(ctx, list)
}

// pruneRow is what Prune needs to decide on one stored log.
type pruneRow struct {
	ID         string    `db:"id"`
	ClientID   int64     `db:"client_id"`
	ReceivedAt time.Time `db:"received_at"`
	S3Key      *string   `db:"s3_key"`
	// ClientGone: the machine was deleted from updater_clients.
	ClientGone bool `db:"client_gone"`
}

// pruneVictims picks the rows to delete: older than cutoff, of a machine
// that no longer exists, or past the newest MaxPerClient of their machine.
// rows must be ordered by client, newest first within a client.
func pruneVictims(rows []pruneRow, cutoff time.Time) []pruneRow {
	var out []pruneRow
	seen := map[int64]int{}
	for _, r := range rows {
		seen[r.ClientID]++
		if r.ClientGone || r.ReceivedAt.Before(cutoff) || seen[r.ClientID] > MaxPerClient {
			out = append(out, r)
		}
	}
	return out
}

// Prune applies MaxPerClient and Retention, and drops the logs of machines
// deleted from updater_clients (which has no foreign key to cascade from
// here on purpose: a cascade would remove the rows and orphan their bucket
// objects). A row is deleted only once its object is.
func (s *Store) Prune(ctx context.Context) (int, error) {
	var rows []pruneRow
	err := s.db.SelectContext(ctx, &rows, `SELECT l.id, l.client_id, l.received_at, l.s3_key, (c.id IS NULL) AS client_gone
		FROM updater_client_logs l LEFT JOIN updater_clients c ON c.id = l.client_id
		ORDER BY l.client_id, l.received_at DESC, l.id DESC`)
	if err != nil {
		return 0, err
	}
	victims := pruneVictims(rows, s.now().Add(-Retention))
	done := 0
	for len(victims) > 0 {
		// S3 DeleteObjects takes at most 1000 keys per call.
		batch := victims[:min(len(victims), pruneBatch)]
		victims = victims[len(batch):]
		keys := make([]*string, len(batch))
		ids := make([]any, len(batch))
		for i, v := range batch {
			keys[i], ids[i] = v.S3Key, v.ID
		}
		if err := s.deleteObjects(ctx, keys); err != nil {
			return done, fmt.Errorf("deleting bucket objects: %w", err)
		}
		q := `DELETE FROM updater_client_logs WHERE id IN (?` + strings.Repeat(",?", len(ids)-1) + `)`
		if _, err := s.db.ExecContext(ctx, q, ids...); err != nil {
			return done, err
		}
		done += len(batch)
	}
	return done, nil
}

const pruneBatch = 500

// Start runs Prune now and then every PruneInterval until ctx is done.
func (s *Store) Start(ctx context.Context) {
	if s == nil || s.db == nil {
		return
	}
	t := time.NewTicker(PruneInterval)
	defer t.Stop()
	for {
		if n, err := s.Prune(ctx); err != nil {
			// The logs simply stay another interval.
			slog.WarnContext(ctx, "client log prune failed", "err", err)
		} else if n > 0 {
			slog.InfoContext(ctx, "client logs pruned", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
