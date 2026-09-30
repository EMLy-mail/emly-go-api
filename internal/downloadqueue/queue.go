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
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultCapacity and DefaultRetryAfter are what New falls back to when
	// the configured value is out of range.
	DefaultCapacity   = 50
	DefaultRetryAfter = 60 * time.Second
	// DefaultDownloadTimeout also applies when there is no queue at all
	// (nil *Queue): the download routes are exempt from main.go's global 30s
	// timeout, so something has to bound them.
	DefaultDownloadTimeout = 10 * time.Minute

	// MaxCapacity, MaxRetryAfter and MaxDownloadTimeout bound what the admin
	// routes accept. They are guard rails against a typo, not tuned limits.
	MaxCapacity        = 10000
	MaxRetryAfter      = 24 * time.Hour
	MaxDownloadTimeout = 24 * time.Hour
)

// ErrEvicted is the cancellation cause of a download an admin removed from
// its slot. The installer stream checks for it so the truncated transfer is
// logged as an eviction rather than as the client disconnecting.
var ErrEvicted = errors.New("download evicted from its queue slot by an admin")

// Why a download that held a slot did not complete. The first four are also
// the "reason" of streamInstaller's "installer download did not complete" log
// line, via FailureReason, so the log and the counters always agree.
const (
	ReasonServerTimeout      = "server timeout"
	ReasonClientDisconnected = "client disconnected"
	ReasonCopyFailed         = "copy failed"
	ReasonEvicted            = "evicted from queue slot"
	// ReasonErrorResponse is a download answered with a 4xx/5xx before any
	// installer byte was sent: unknown version, file missing from S3, S3 down.
	ReasonErrorResponse = "error response"
	// ReasonInternalError is a handler that panicked mid-download.
	ReasonInternalError = "internal error"
)

// FailureReason names why the download whose request context is ctx stopped
// short: evicted by an admin, cut by the download timeout (the queue's
// DownloadTimeout), abandoned by the client, or - with a live context - a
// plain copy failure.
func FailureReason(ctx context.Context) string {
	switch {
	case errors.Is(context.Cause(ctx), ErrEvicted):
		return ReasonEvicted
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return ReasonServerTimeout
	case errors.Is(ctx.Err(), context.Canceled):
		return ReasonClientDisconnected
	}
	return ReasonCopyFailed
}

// Settings is the queue's tunable state: what .env provides at startup and
// what the admin routes change at runtime.
//
// DownloadTimeout is how long one installer download may take end to end,
// replacing the global 30s request timeout on those two routes. It applies
// whether or not the slot limit is enabled, and is fixed per download when
// it starts: changing it affects new downloads, not ones in flight.
type Settings struct {
	Enabled         bool
	Capacity        int
	RetryAfter      time.Duration
	DownloadTimeout time.Duration
}

// SlotInfo describes one download holding a slot. The caller fills in who
// and what; the rest is the queue's bookkeeping, filled in by Status.
//
// AvgBytesPerSec is bytes sent divided by the time since StartedAt - an
// average over the whole transfer so far, not an instantaneous rate, and it
// includes the release lookup and S3 open that precede the first byte.
// BytesTotal and Percent are only set once the installer's Content-Length is
// known. DeadlineAt is when the download timeout will cut the transfer.
type SlotInfo struct {
	ID             uint64    `json:"id"`
	Product        string    `json:"product"`
	Version        string    `json:"version"`
	IP             string    `json:"ip,omitempty"`
	Hostname       string    `json:"hostname,omitempty"`
	HWID           string    `json:"hwid,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	DeadlineAt     time.Time `json:"deadline_at"`
	ElapsedSeconds float64   `json:"elapsed_seconds"`
	BytesSent      int64     `json:"bytes_sent"`
	BytesTotal     int64     `json:"bytes_total,omitempty"`
	Percent        float64   `json:"percent,omitempty"`
	AvgBytesPerSec int64     `json:"avg_bytes_per_sec"`
}

// SettingsView is Settings as the admin routes serialize it.
type SettingsView struct {
	Enabled                bool `json:"enabled"`
	Capacity               int  `json:"capacity"`
	RetryAfterSeconds      int  `json:"retry_after_seconds"`
	DownloadTimeoutSeconds int  `json:"download_timeout_seconds"`
}

// Status is a point-in-time snapshot of the queue.
//
// Every download that took a slot ends up in exactly one of CompletedTotal
// (the whole installer was sent), FailedTotal (it was not - FailedByReason
// says why) or EvictedTotal (an admin stopped it); until then it is counted in
// Active. A download refused at the door is only in RejectedTotal.
type Status struct {
	SettingsView
	Active         int               `json:"active"`
	Available      int               `json:"available"`
	CompletedTotal uint64            `json:"completed_total"`
	FailedTotal    uint64            `json:"failed_total"`
	FailedByReason map[string]uint64 `json:"failed_by_reason"`
	RejectedTotal  uint64            `json:"rejected_total"`
	EvictedTotal   uint64            `json:"evicted_total"`
	// TotalBytesPerSec is the sum of every slot's AvgBytesPerSec: roughly
	// the bandwidth installer downloads are using right now.
	TotalBytesPerSec int64        `json:"total_bytes_per_sec"`
	Defaults         SettingsView `json:"defaults"`
	Slots            []SlotInfo   `json:"slots"`
}

// Patch is a partial settings change; a nil field is left as it is.
type Patch struct {
	Enabled                *bool `json:"enabled"`
	Capacity               *int  `json:"capacity"`
	RetryAfterSeconds      *int  `json:"retry_after_seconds"`
	DownloadTimeoutSeconds *int  `json:"download_timeout_seconds"`
}

// Empty reports whether p changes nothing.
func (p Patch) Empty() bool {
	return p.Enabled == nil && p.Capacity == nil && p.RetryAfterSeconds == nil && p.DownloadTimeoutSeconds == nil
}

// ErrInvalidPatch wraps every validation failure of Update.
var ErrInvalidPatch = errors.New("invalid download queue settings")

type slot struct {
	info    SlotInfo
	cancel  context.CancelCauseFunc
	timeout time.Duration
	// sent and total are written by the download's goroutine and read by
	// Status, hence atomic rather than under q.mu.
	sent  atomic.Int64
	total atomic.Int64
}

// addSent and setTotal are safe on a nil slot, which is what a nil queue's
// middleware would hold.
func (s *slot) addSent(n int) {
	if s != nil {
		s.sent.Add(int64(n))
	}
}

func (s *slot) setTotal(n int64) {
	if s != nil && n > 0 {
		s.total.Store(n)
	}
}

// Queue is the slot counter. The zero value is not usable; call New. A nil
// *Queue is: every method treats it as a queue that is off and tracks nothing.
type Queue struct {
	mu        sync.Mutex
	defaults  Settings
	current   Settings
	nextID    uint64
	active    map[uint64]*slot
	completed uint64
	failed    map[string]uint64
	rejected  uint64
	evicted   uint64
	now       func() time.Time
}

// New builds a queue from the startup settings, replacing an out-of-range
// capacity or retry interval with its default.
func New(s Settings) *Queue {
	s = normalize(s)
	return &Queue{
		defaults: s,
		current:  s,
		active:   make(map[uint64]*slot),
		failed:   make(map[string]uint64),
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
	if s.DownloadTimeout < time.Second || s.DownloadTimeout > MaxDownloadTimeout {
		s.DownloadTimeout = DefaultDownloadTimeout
	}
	return s
}

// TryAcquire takes a slot for info. It fails only when the queue is enabled
// and already holds capacity downloads; a disabled queue always succeeds and
// still records the slot. cancel is kept so Evict can stop the transfer. The
// returned id is what Release and Evict take.
func (q *Queue) TryAcquire(info SlotInfo, cancel context.CancelCauseFunc) (uint64, bool) {
	s, ok := q.acquire(info, cancel)
	if s == nil {
		return 0, ok
	}
	return s.info.ID, ok
}

// acquire is TryAcquire returning the slot itself, so the middleware can
// count bytes into it without a map lookup per write.
func (q *Queue) acquire(info SlotInfo, cancel context.CancelCauseFunc) (*slot, bool) {
	if q == nil {
		return nil, true
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.current.Enabled && len(q.active) >= q.current.Capacity {
		q.rejected++
		return nil, false
	}
	q.nextID++
	info.ID = q.nextID
	info.StartedAt = q.now().UTC()
	info.DeadlineAt = info.StartedAt.Add(q.current.DownloadTimeout)
	s := &slot{info: info, cancel: cancel, timeout: q.current.DownloadTimeout}
	q.active[info.ID] = s
	return s, true
}

// Release frees a slot as a completed download. Releasing an id that is not
// held - already evicted, or 0 from a nil queue - is a no-op, so a handler can
// always defer it.
func (q *Queue) Release(id uint64) {
	q.Finish(id, "")
}

// Finish frees a slot and records how its download ended: reason "" means
// the installer was sent in full, anything else counts as a failure under that
// reason. Like Release, a slot no longer held is a no-op - an evicted
// download was already counted as evicted and must not count twice.
func (q *Queue) Finish(id uint64, reason string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.active[id]; !ok {
		return
	}
	delete(q.active, id)
	if reason == "" {
		q.completed++
	} else {
		q.failed[reason]++
	}
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
// until the count drops under the new capacity. Likewise a new download
// timeout only applies to downloads that start after it.
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
	if p.DownloadTimeoutSeconds != nil && (*p.DownloadTimeoutSeconds < 1 || time.Duration(*p.DownloadTimeoutSeconds)*time.Second > MaxDownloadTimeout) {
		return Status{}, errors.Join(ErrInvalidPatch, errors.New("download_timeout_seconds must be between 1 and 86400"))
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
	if p.DownloadTimeoutSeconds != nil {
		q.current.DownloadTimeout = time.Duration(*p.DownloadTimeoutSeconds) * time.Second
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
		return Status{FailedByReason: map[string]uint64{}, Slots: []SlotInfo{}}
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	now := q.now().UTC()
	slots := make([]SlotInfo, 0, len(q.active))
	var totalRate int64
	for _, s := range q.active {
		info := s.snapshot(now)
		totalRate += info.AvgBytesPerSec
		slots = append(slots, info)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].ID < slots[j].ID })

	failedBy := make(map[string]uint64, len(q.failed))
	var failedTotal uint64
	for reason, n := range q.failed {
		failedBy[reason] = n
		failedTotal += n
	}

	return Status{
		SettingsView:   view(q.current),
		Active:         len(q.active),
		Available:      max(q.current.Capacity-len(q.active), 0),
		CompletedTotal: q.completed,
		FailedTotal:    failedTotal,
		FailedByReason: failedBy,
		RejectedTotal:  q.rejected,
		EvictedTotal:   q.evicted,

		TotalBytesPerSec: totalRate,
		Defaults:         view(q.defaults),
		Slots:            slots,
	}
}

func (s *slot) snapshot(now time.Time) SlotInfo {
	info := s.info
	info.BytesSent = s.sent.Load()
	info.BytesTotal = s.total.Load()
	elapsed := now.Sub(info.StartedAt).Seconds()
	info.ElapsedSeconds = math.Round(elapsed*10) / 10
	if elapsed > 0 {
		info.AvgBytesPerSec = int64(float64(info.BytesSent) / elapsed)
	}
	if info.BytesTotal > 0 {
		info.Percent = math.Round(float64(info.BytesSent)/float64(info.BytesTotal)*1000) / 10
	}
	return info
}

func view(s Settings) SettingsView {
	return SettingsView{
		Enabled:                s.Enabled,
		Capacity:               s.Capacity,
		RetryAfterSeconds:      seconds(s.RetryAfter),
		DownloadTimeoutSeconds: seconds(s.DownloadTimeout),
	}
}

func seconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}
