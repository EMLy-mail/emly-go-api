package clienthub

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// snapshotFormat is bumped when the file layout changes incompatibly; a file
// with any other value is refused by LoadEvents rather than half-read.
const snapshotFormat = 1

// eventSnapshot is the on-disk form of h.events: gzip'd JSON, so it can be
// inspected with `zcat | jq` and a field added to EventRecord later does not
// make older files unreadable.
type eventSnapshot struct {
	Format  int                     `json:"format"`
	SavedAt time.Time               `json:"saved_at"`
	Events  map[int64][]EventRecord `json:"events"`
}

// SaveEvents writes every client's event ring to path, and reports whether
// it wrote anything: a hub whose events have not changed since the last save
// is skipped, so a periodic caller costs nothing on a quiet fleet.
//
// The file is written next to path and renamed over it, so a crash or a full
// disk mid-write leaves the previous snapshot intact instead of a truncated
// one. Only events are saved - sessions belong to live sockets and commands
// are a short-lived debugging aid, neither is meaningful after a restart.
func (h *Hub) SaveEvents(path string) (bool, error) {
	if h == nil {
		return false, nil
	}
	h.saveMu.Lock()
	defer h.saveMu.Unlock()

	h.mu.Lock()
	ver := h.eventsVer
	if ver == h.savedVer {
		h.mu.Unlock()
		return false, nil
	}
	// The rings are copied under the lock and encoded after it is released:
	// RecordEvent only ever appends to or reslices a ring, never writes into
	// an element already stored, so the copied slices stay valid.
	snap := eventSnapshot{Format: snapshotFormat, SavedAt: h.now().UTC(), Events: make(map[int64][]EventRecord, len(h.events))}
	for id, ring := range h.events {
		snap.Events[id] = append([]EventRecord(nil), ring...)
	}
	h.mu.Unlock()

	if err := writeSnapshot(path, snap); err != nil {
		return false, err
	}

	h.mu.Lock()
	h.savedVer = ver
	h.mu.Unlock()
	return true, nil
}

func writeSnapshot(path string, snap eventSnapshot) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	// Removing after a successful rename fails harmlessly: the name is gone.
	defer func() { _ = os.Remove(tmp.Name()) }()

	gz := gzip.NewWriter(tmp)
	if err := json.NewEncoder(gz).Encode(snap); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadEvents restores the event rings saved by SaveEvents and returns how
// many clients it restored. A missing file is not an error (first start, or
// persistence just enabled): it returns 0, nil. Call it once at startup,
// before any connection is accepted.
//
// What comes back goes through the same limits RecordEvent applies, so a file
// written by a build with larger limits cannot smuggle past them: rings are
// cut to eventRingSize, oversized payloads are dropped, clients with nothing
// newer than recordRetention are skipped (Prune would drop them anyway) and
// at most maxEventClients are kept. Events already in the hub stay newest.
func (h *Hub) LoadEvents(path string) (int, error) {
	if h == nil {
		return 0, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("client events snapshot %s: %w", path, err)
	}
	var snap eventSnapshot
	if err := json.NewDecoder(gz).Decode(&snap); err != nil {
		return 0, fmt.Errorf("client events snapshot %s: %w", path, err)
	}
	if snap.Format != snapshotFormat {
		return 0, fmt.Errorf("client events snapshot %s: unsupported format %d", path, snap.Format)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := h.now().Add(-recordRetention)
	restored := 0
	for id, ring := range snap.Events {
		if len(ring) == 0 || ring[len(ring)-1].ReceivedAt.Before(cutoff) {
			continue
		}
		for i := range ring {
			ring[i].ClientID = id
			if len(ring[i].Payload) > eventPayloadCapBytes {
				ring[i].Payload = nil
				ring[i].Truncated = true
			}
		}
		_, existed := h.events[id]
		merged := append(ring, h.events[id]...)
		if len(merged) > eventRingSize {
			merged = merged[len(merged)-eventRingSize:]
		}
		h.events[id] = merged
		if !existed && len(h.events) > maxEventClients {
			h.evictLeastRecentEventsClientLocked()
		}
		restored++
	}
	h.eventsVer++
	return restored, nil
}
