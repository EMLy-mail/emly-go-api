// Package statshub is the in-process event bus behind GET /v2/stats/stream
// (docs/superpowers/specs/2026-09-04-websocket-stats-stream-design.md): it
// fans out "something changed" notifications from the updater-event ingest
// path (recordUpdaterEvent in internal/handlers) to every open stats WS
// connection, plus a periodic tick so time-derived fields (connected client
// counts) stay fresh without a new event.
//
// It is HTTP- and DB-free like internal/remoteconfig: Hub only carries typed
// notifications, never queries the database itself. internal/handlers is its
// only caller - it decides what to fetch and how to shape it into a WS
// message.
//
// Multi-instance note (design doc §8): this is an in-process bus only, so an
// event ingested by one API replica never reaches a stats WS connection held
// open on another. That is deliberately out of scope for v1 - the project
// has neither Postgres LISTEN/NOTIFY nor Redis pub/sub available, and the
// expected fleet size (a few hundred updater clients) does not call for
// either just to support this. If the API ever runs multi-instance behind a
// load balancer, revisit this the same way configmirror documents its own
// single-instance assumptions.
package statshub

import (
	"context"
	"sync"
	"time"

	"emly-api-go/internal/models"
)

// EventKind tells a Hub subscriber what shape Event carries.
type EventKind string

const (
	// EventKindUpdaterEvent fires once per ingested updater_events row
	// (manifest_check or download, either product). Client and EventEntry
	// are both set.
	EventKindUpdaterEvent EventKind = "updater_event"
	// EventKindTick fires on Hub's own timer (Run), independent of any
	// ingest, so subscribers can refresh time-derived fields such as
	// "connected in the last N minutes" that go stale by the clock alone.
	EventKindTick EventKind = "tick"
)

// Event is one notification pushed through the Hub. Which fields are set
// depends on Kind.
type Event struct {
	Kind EventKind

	// Set only for EventKindUpdaterEvent. Client is the full row for the
	// client that produced EventEntry, fetched fresh at ingest time so a
	// subscriber can push it straight into a stats:clients delta without a
	// query of its own.
	Client     *models.UpdaterClient
	EventEntry *models.UpdaterEvent
}

// subscriber is one Hub.Subscribe() registration. The channel is buffered
// and best-effort: per the design doc §7, this is a refresh channel with no
// delivery guarantee, so a slow or stuck subscriber has its event dropped
// rather than blocking Publish for everyone else.
type subscriber struct {
	ch chan Event
}

const subscriberBuffer = 16

// Hub fans out Event values to every current subscriber. The zero value is
// not usable; construct with New. Safe for concurrent use.
type Hub struct {
	mu   sync.RWMutex
	subs map[*subscriber]struct{}
}

// New returns an empty, ready-to-use Hub.
func New() *Hub {
	return &Hub{subs: make(map[*subscriber]struct{})}
}

// Subscribe registers a new listener and returns its event channel plus a
// cancel func that unregisters it. Callers must call cancel exactly once
// (typically deferred) when done listening, and must keep draining the
// channel until then so Publish never blocks on them for long - Publish
// itself never blocks (see Publish), but an unsubscribed-yet-undrained
// channel would leak.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, subscriberBuffer)}

	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		delete(h.subs, sub)
		h.mu.Unlock()
	}
	return sub.ch, cancel
}

// Active reports whether at least one subscriber is currently listening.
// recordUpdaterEvent checks this before doing the extra client-row fetch a
// Publish needs, so a quiet server with no dashboard connected pays nothing
// extra per updater request.
func (h *Hub) Active() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs) > 0
}

// Publish fans ev out to every current subscriber without blocking: a
// subscriber whose buffer is full has this event dropped for it (logged by
// nothing - by design, per §7 this channel resyncs via the next snapshot,
// never guarantees at-least-once delivery).
func (h *Hub) Publish(ev Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.subs {
		select {
		case sub.ch <- ev:
		default:
		}
	}
}

// Run ticks EventKindTick into Publish every interval until ctx is done.
// Call it once from a long-lived background goroutine (main.go), the same
// way configmirror.Start runs its own poll loop.
func (h *Hub) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.Publish(Event{Kind: EventKindTick})
		}
	}
}
