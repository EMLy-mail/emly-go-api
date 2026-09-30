// Package downloadqueue bounds how many installer downloads - the EMLy app's
// GET /v2/updates/releases/{version}/download and the EMLy Updater's
// GET /v2/updates/download/updater/{version} - may stream at the same time.
//
// Despite the name there is no waiting line: a download either takes a free
// slot at once or is refused with 429 and a Retry-After, and the client comes
// back later. Holding a request open while it waits would tie up the very
// connection the limit exists to protect, and the updater already retries on
// its own schedule.
//
// State lives in memory only - no table, nothing persisted - like
// internal/presencehub and internal/statshub, and with the same
// single-instance limit: each API replica counts its own downloads, and a
// capacity changed through the admin routes lasts until the next restart,
// which goes back to the DOWNLOAD_QUEUE_* values in .env.
//
// Slots are tracked even while the queue is disabled, so the dashboard can see
// how many downloads are in flight before deciding to switch the limit on.
package downloadqueue

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

const (
	// DefaultCapacity and DefaultRetryAfter are what New falls back to when
	// the configured value is out of range.
	DefaultCapacity   = 50
	DefaultRetryAfter = 60 * time.Second

	// MaxCapacity and MaxRetryAfter bound what the admin routes accept. They
	// are guard rails against a typo, not tuned limits.
	MaxCapacity   = 10000
	MaxRetryAfter = 24 * time.Hour
)

// ErrEvicted is the cancellation cause of a download an admin removed from
// its slot. The installer stream checks for it so the truncated transfer is
// logged as an eviction rather than as the client disconnecting.
var ErrEvicted = errors.New("download evicted from its queue slot by an admin")

// Settings is the queue's tunable state: what .env provides at startup and
// what the admin routes change at runtime.
type Settings struct {
	Enabled    bool
	Capacity   int
	RetryAfter time.Duration
}

// SlotInfo describes one download holding a slot.
type SlotInfo struct {
	ID        uint64    `json:"id"`
	Product   string    `json:"product"`
	Version   string    `json:"version"`
	IP        string    `json:"ip,omitempty"`
	Hostname  string    `json:"hostname,omitempty"`
	HWID      string    `json:"hwid,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// SettingsView is Settings as the admin routes serialize it.
type SettingsView struct {
	Enabled           bool `json:"enabled"`
	Capacity          int  `json:"capacity"`
	RetryAfterSeconds int  `json:"retry_after_seconds"`
}

// Status is a point-in-time snapshot of the queue.
type Status struct {
	SettingsView
	Active        int          `json:"active"`
	Available     int          `json:"available"`
	AcquiredTotal uint64       `json:"acquired_total"`
	RejectedTotal uint64       `json:"rejected_total"`
	EvictedTotal  uint64       `json:"evicted_total"`
	Defaults      SettingsView `json:"defaults"`
	Slots         []SlotInfo   `json:"slots"`
}

// Patch is a partial settings change; a nil field is left as it is.
type Patch struct {
	Enabled           *bool `json:"enabled"`
	Capacity          *int  `json:"capacity"`
	RetryAfterSeconds *int  `json:"retry_after_seconds"`
}

// ErrInvalidPatch wraps every validation failure of Update.
var ErrInvalidPatch = errors.New("invalid download queue settings")

type slot struct {
	info   SlotInfo
	cancel context.CancelCauseFunc
}

// Queue is the slot counter. The zero value is not usable; call New. A nil
// *Queue is: every method treats it as a queue that is off and tracks nothing.
type Queue struct {
	mu       sync.Mutex
	defaults Settings
	current  Settings
	nextID   uint64
	active   map[uint64]*slot
	acquired uint64
	rejected uint64
	evicted  uint64
	now      func() time.Time
}

// New builds a queue from the startup settings, replacing an out-of-range
// capacity or retry interval with its default.
func New(s Settings) *Queue {
	s = normalize(s)
	return &Queue{
		defaults: s,
		current:  s,
		active:   make(map[uint64]*slot),
		now:      time.Now,
	}
}

func normalize(s Settings) Settings {
	if s.Capacity < 1 || s.Capacity > MaxCapacity {
		s.Capacity = DefaultCapacity
	}
	if s.RetryAfter < time.Second || s.RetryAfter > MaxRetryAfter {
		s.RetryAfter = DefaultRetryAfter
	}
	return s
}

// TryAcquire takes a slot for info. It fails only when the queue is enabled
// and already holds capacity downloads; a disabled queue always succeeds and
// still records the slot. cancel is kept so Evict can stop the transfer. The
// returned id is what Release and Evict take.
func (q *Queue) TryAcquire(info SlotInfo, cancel context.CancelCauseFunc) (uint64, bool) {
	if q == nil {
		return 0, true
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.current.Enabled && len(q.active) >= q.current.Capacity {
		q.rejected++
		return 0, false
	}
	q.nextID++
	info.ID = q.nextID
	info.StartedAt = q.now().UTC()
	q.active[info.ID] = &slot{info: info, cancel: cancel}
	q.acquired++
	return info.ID, true
}

// Release frees a slot. Releasing an id that is not held - already evicted,
// or 0 from a nil queue - is a no-op, so a handler can always defer it.
func (q *Queue) Release(id uint64) {
	if q == nil {
		return
	}
	q.mu.Lock()
	delete(q.active, id)
	q.mu.Unlock()
}

// Evict frees a slot immediately and cancels its download with ErrEvicted.
// It reports whether the slot was held.
func (q *Queue) Evict(id uint64) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	s, ok := q.active[id]
	if ok {
		delete(q.active, id)
		q.evicted++
	}
	q.mu.Unlock()
	if ok && s.cancel != nil {
		s.cancel(ErrEvicted)
	}
	return ok
}

// EvictAll empties every slot and returns how many there were.
func (q *Queue) EvictAll() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	slots := q.active
	q.active = make(map[uint64]*slot)
	q.evicted += uint64(len(slots))
	q.mu.Unlock()
	for _, s := range slots {
		if s.cancel != nil {
			s.cancel(ErrEvicted)
		}
	}
	return len(slots)
}

// Update applies a partial settings change. Shrinking below the number of
// downloads in flight evicts nobody: they finish, and new ones are refused
// until the count drops under the new capacity.
func (q *Queue) Update(p Patch) (Status, error) {
	if q == nil {
		return Status{}, errors.New("download queue is not configured")
	}
	if p.Capacity != nil && (*p.Capacity < 1 || *p.Capacity > MaxCapacity) {
		return Status{}, errors.Join(ErrInvalidPatch, errors.New("capacity must be between 1 and 10000"))
	}
	if p.RetryAfterSeconds != nil && (*p.RetryAfterSeconds < 1 || time.Duration(*p.RetryAfterSeconds)*time.Second > MaxRetryAfter) {
		return Status{}, errors.Join(ErrInvalidPatch, errors.New("retry_after_seconds must be between 1 and 86400"))
	}

	q.mu.Lock()
	if p.Enabled != nil {
		q.current.Enabled = *p.Enabled
	}
	if p.Capacity != nil {
		q.current.Capacity = *p.Capacity
	}
	if p.RetryAfterSeconds != nil {
		q.current.RetryAfter = time.Duration(*p.RetryAfterSeconds) * time.Second
	}
	q.mu.Unlock()
	return q.Status(), nil
}

// Reset restores the settings New was given (the .env values). Downloads in
// flight keep their slots.
func (q *Queue) Reset() Status {
	if q == nil {
		return Status{}
	}
	q.mu.Lock()
	q.current = q.defaults
	q.mu.Unlock()
	return q.Status()
}

// RetryAfterSeconds is the Retry-After a refused download is given, rounded
// up to a whole second.
func (q *Queue) RetryAfterSeconds() int {
	if q == nil {
		return seconds(DefaultRetryAfter)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return seconds(q.current.RetryAfter)
}

// Status returns a snapshot, slots ordered oldest first.
func (q *Queue) Status() Status {
	if q == nil {
		return Status{Slots: []SlotInfo{}}
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	slots := make([]SlotInfo, 0, len(q.active))
	for _, s := range q.active {
		slots = append(slots, s.info)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].ID < slots[j].ID })

	return Status{
		SettingsView:  view(q.current),
		Active:        len(q.active),
		Available:     max(q.current.Capacity-len(q.active), 0),
		AcquiredTotal: q.acquired,
		RejectedTotal: q.rejected,
		EvictedTotal:  q.evicted,
		Defaults:      view(q.defaults),
		Slots:         slots,
	}
}

func view(s Settings) SettingsView {
	return SettingsView{Enabled: s.Enabled, Capacity: s.Capacity, RetryAfterSeconds: seconds(s.RetryAfter)}
}

func seconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}
