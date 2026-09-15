// Package logfile writes the API's log output to one file per calendar day,
// named after the moment it was opened:
//
//	emly-api-log-2026-09-15-14-30-05.log
//
// The time part uses '-' rather than ':' because ':' is not a legal file name
// character on Windows (on NTFS "a:b" silently creates an alternate data
// stream instead of a file), and the API is built and run there too.
//
// A Writer opens its first file when constructed and moves to a new one on
// the first write of a new day (process-local time zone, so TZ decides where
// the day boundary falls). A restart opens a new file as well: the timestamp
// keeps two runs on the same day apart instead of interleaving them. Files
// older than the retention window are pruned whenever a new file is opened.
//
// Like internal/statshub and internal/remoteconfig it is HTTP- and DB-free;
// main.go is its only caller.
package logfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	filePrefix  = "emly-api-log-"
	fileExt     = ".log"
	stampLayout = "2006-01-02-15-04-05"
	dayLayout   = "2006-01-02"

	// rotateRetry is how long a Writer keeps using the previous day's file
	// after failing to open a new one, before trying again. Retrying on every
	// write would turn one full disk into an error line per log line.
	rotateRetry = time.Minute
)

// Writer is an io.Writer over the current day's log file. It is safe for
// concurrent use.
type Writer struct {
	dir           string
	retentionDays int
	now           func() time.Time

	mu      sync.Mutex
	file    *os.File
	path    string
	day     string
	retryAt time.Time
}

// Open creates dir if needed and opens today's log file in it. retentionDays
// is how many days of files to keep besides today's; 0 keeps every file.
func Open(dir string, retentionDays int) (*Writer, error) {
	return open(dir, retentionDays, time.Now)
}

func open(dir string, retentionDays int, now func() time.Time) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir %q: %w", dir, err)
	}
	w := &Writer{dir: dir, retentionDays: retentionDays, now: now}
	if err := w.rotate(now()); err != nil {
		return nil, err
	}
	return w, nil
}

// Write appends p to the current day's file, opening a new file first when
// the day has changed since the last write.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return 0, os.ErrClosed
	}

	now := w.now()
	if now.Format(dayLayout) != w.day && !now.Before(w.retryAt) {
		if err := w.rotate(now); err != nil {
			// Keep the line in yesterday's file rather than dropping it.
			w.retryAt = now.Add(rotateRetry)
			fmt.Fprintf(os.Stderr, "logfile: %v\n", err)
		}
	}
	return w.file.Write(p)
}

// Path returns the file currently being written to.
func (w *Writer) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path
}

// Close closes the current file. Writes after Close fail with os.ErrClosed.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// rotate opens the file for now and makes it current. The previous file is
// closed only once the new one is open, so a failure leaves w writable.
func (w *Writer) rotate(now time.Time) error {
	path := filepath.Join(w.dir, filePrefix+now.Format(stampLayout)+fileExt)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file, w.path, w.day = f, path, now.Format(dayLayout)
	w.retryAt = time.Time{}

	if err := w.prune(now); err != nil {
		fmt.Fprintf(os.Stderr, "logfile: %v\n", err)
	}
	return nil
}

// prune removes log files whose day is more than retentionDays before now's.
// Only names this package generates are considered, so anything else an
// operator keeps in the same directory is left alone.
func (w *Writer) prune(now time.Time) error {
	if w.retentionDays <= 0 {
		return nil
	}

	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return fmt.Errorf("prune log dir: %w", err)
	}

	y, m, d := now.Date()
	cutoff := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -w.retentionDays)

	var errs []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileExt) {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileExt)
		opened, err := time.ParseInLocation(stampLayout, stamp, now.Location())
		if err != nil || !opened.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(w.dir, name)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
