package eventprune

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
)

const (
	// DefaultArchivePrefix is the folder used when Options.Prefix is empty,
	// next to clientlogs' emly-api-files/client-logs.
	DefaultArchivePrefix = "emly-api-files/archive/updater_events"

	// rowsPerInsert bounds one INSERT statement in a dump, so restoring a
	// file never trips max_allowed_packet however large the batch.
	rowsPerInsert = 500
)

// Archiver is the part of storage.S3Connector the archive uses, so tests can
// stand in for the bucket.
type Archiver interface {
	UploadFile(ctx context.Context, key string, body io.Reader, contentType string, metadata map[string]string) (string, error)
}

// archiveBatch dumps up to batchSize rows below firstKeptID to the bucket and
// deletes exactly those rows, returning how many it removed (0 = nothing
// left). The delete only runs once the upload has succeeded: a bucket that is
// down leaves the rows where they are, for the next hourly pass.
func archiveBatch(ctx context.Context, db *sqlx.DB, store Archiver, prefix string, firstKeptID int64) (int64, error) {
	rows, err := db.QueryxContext(ctx,
		`SELECT * FROM updater_events WHERE id < ? ORDER BY id ASC LIMIT ?`, firstKeptID, batchSize)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	var data [][]any
	for rows.Next() {
		vals, err := rows.SliceScan()
		if err != nil {
			return 0, err
		}
		data = append(data, vals)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()
	if len(data) == 0 {
		return 0, nil
	}

	idCol, createdCol := indexOf(cols, "id"), indexOf(cols, "created_at")
	if idCol < 0 {
		return 0, fmt.Errorf("updater_events has no id column")
	}
	minID, err := asInt64(data[0][idCol])
	if err != nil {
		return 0, err
	}
	maxID, err := asInt64(data[len(data)-1][idCol])
	if err != nil {
		return 0, err
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := writeDump(gz, "updater_events", cols, data, time.Now().UTC()); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}

	// Filed under the day of the oldest row, so finding "the events of
	// March 3rd" is a prefix listing rather than opening every file.
	day := time.Now().UTC()
	if createdCol >= 0 {
		if t, ok := data[0][createdCol].(time.Time); ok {
			day = t.UTC()
		}
	}
	key := archiveKey(prefix, day, minID, maxID)
	if _, err := store.UploadFile(ctx, key, &buf, "application/gzip", map[string]string{
		"rows":   strconv.Itoa(len(data)),
		"min-id": strconv.FormatInt(minID, 10),
		"max-id": strconv.FormatInt(maxID, 10),
	}); err != nil {
		return 0, err
	}

	// The range is exactly the rows just dumped: they were read in id order
	// below firstKeptID, where nothing new is ever inserted.
	res, err := db.ExecContext(ctx, `DELETE FROM updater_events WHERE id BETWEEN ? AND ?`, minID, maxID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// archiveKey is the object name of one dump: <prefix>/YYYY/MM/DD/
// updater_events_<min id>-<max id>.sql.gz, with an empty prefix meaning
// DefaultArchivePrefix.
func archiveKey(prefix string, day time.Time, minID, maxID int64) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		prefix = DefaultArchivePrefix
	}
	return fmt.Sprintf("%s/%s/updater_events_%d-%d.sql.gz", prefix, day.Format("2006/01/02"), minID, maxID)
}

// writeDump writes rows as a restorable SQL script. INSERT IGNORE because a
// restore is a repair: a row still present, or one whose updater_clients row
// has since been deleted, is skipped instead of aborting the whole file.
// time_zone is pinned to UTC because TIMESTAMP literals are read in the
// session's zone, and the values are written in UTC.
func writeDump(w io.Writer, table string, cols []string, data [][]any, generatedAt time.Time) error {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = "`" + strings.ReplaceAll(c, "`", "``") + "`"
	}
	header := fmt.Sprintf("-- emly-api-go: %d rows archived from `%s` before deletion\n-- generated %s\nSET time_zone = '+00:00';\n",
		len(data), table, generatedAt.Format(time.RFC3339))
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}
	for start := 0; start < len(data); start += rowsPerInsert {
		end := min(start+rowsPerInsert, len(data))
		var sb strings.Builder
		fmt.Fprintf(&sb, "INSERT IGNORE INTO `%s` (%s) VALUES\n", table, strings.Join(quoted, ", "))
		for i, row := range data[start:end] {
			if i > 0 {
				sb.WriteString(",\n")
			}
			sb.WriteByte('(')
			for j, v := range row {
				if j > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(sqlLiteral(v))
			}
			sb.WriteByte(')')
		}
		sb.WriteString(";\n")
		if _, err := io.WriteString(w, sb.String()); err != nil {
			return err
		}
	}
	return nil
}

// sqlLiteral renders one scanned value as a MySQL literal. Text is escaped
// the way mysqldump does; bytes that are not valid UTF-8 become a hex
// literal, so nothing is mangled by the connection charset on restore.
func sqlLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case time.Time:
		return "'" + x.UTC().Format("2006-01-02 15:04:05.999999") + "'"
	case []byte:
		if !utf8.Valid(x) {
			return "X'" + hex.EncodeToString(x) + "'"
		}
		return quoteString(string(x))
	case string:
		return quoteString(x)
	default:
		return quoteString(fmt.Sprint(x))
	}
}

var sqlEscaper = strings.NewReplacer(
	`\`, `\\`, `'`, `\'`, `"`, `\"`, "\x00", `\0`, "\n", `\n`, "\r", `\r`, "\x1a", `\Z`,
)

func quoteString(s string) string { return "'" + sqlEscaper.Replace(s) + "'" }

func asInt64(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case uint64:
		return int64(x), nil
	case []byte:
		return strconv.ParseInt(string(x), 10, 64)
	case string:
		return strconv.ParseInt(x, 10, 64)
	}
	return 0, fmt.Errorf("unexpected id type %T", v)
}

func indexOf(cols []string, name string) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	return -1
}
