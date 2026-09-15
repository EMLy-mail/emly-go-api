package logfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// clock is a settable time source for driving day changes in tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestOpenNamesFileAfterOpeningTime(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	c := &clock{t: time.Date(2026, 9, 15, 14, 30, 5, 0, time.Local)}

	w, err := open(dir, 0, c.now)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer w.Close()

	want := filepath.Join(dir, "emly-api-log-2026-09-15-14-30-05.log")
	if w.Path() != want {
		t.Fatalf("path = %q, want %q", w.Path(), want)
	}
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readFile(t, want); got != "hello\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestWriteRotatesOnDayChange(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 9, 15, 23, 59, 0, 0, time.Local)}

	w, err := open(dir, 0, c.now)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer w.Close()
	first := w.Path()

	w.Write([]byte("a\n"))
	c.t = c.t.Add(30 * time.Second) // still the same day
	w.Write([]byte("b\n"))
	if w.Path() != first {
		t.Fatalf("rotated within the same day: %q -> %q", first, w.Path())
	}

	c.t = time.Date(2026, 9, 16, 0, 0, 12, 0, time.Local)
	w.Write([]byte("c\n"))

	second := filepath.Join(dir, "emly-api-log-2026-09-16-00-00-12.log")
	if w.Path() != second {
		t.Fatalf("path after midnight = %q, want %q", w.Path(), second)
	}
	if got := readFile(t, first); got != "a\nb\n" {
		t.Fatalf("first file = %q", got)
	}
	if got := readFile(t, second); got != "c\n" {
		t.Fatalf("second file = %q", got)
	}
}

func TestRetentionPrunesOnlyOldLogFiles(t *testing.T) {
	dir := t.TempDir()
	keep := []string{
		"emly-api-log-2026-09-08-09-00-00.log", // exactly 7 days before
		"emly-api-log-2026-09-14-18-00-00.log",
		"app.log",                          // not ours
		"emly-api-log-not-a-stamp.log",     // not ours
		"emly-api-log-2020-01-01-00-00-00", // no extension, not ours
	}
	drop := []string{
		"emly-api-log-2026-09-07-23-59-59.log",
		"emly-api-log-2026-01-01-00-00-00.log",
	}
	for _, name := range append(append([]string{}, keep...), drop...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c := &clock{t: time.Date(2026, 9, 15, 10, 0, 0, 0, time.Local)}
	w, err := open(dir, 7, c.now)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer w.Close()

	for _, name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
	for _, name := range drop {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was kept (err=%v)", name, err)
		}
	}
}

func TestZeroRetentionKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "emly-api-log-2000-01-01-00-00-00.log")
	if err := os.WriteFile(old, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := open(dir, 0, (&clock{t: time.Date(2026, 9, 15, 0, 0, 0, 0, time.Local)}).now)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer w.Close()

	if _, err := os.Stat(old); err != nil {
		t.Fatalf("old file removed with retention 0: %v", err)
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	w, err := Open(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := w.Write([]byte("late\n")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write after close err = %v, want os.ErrClosed", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
