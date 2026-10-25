// Package presencehub tracks which EMLy Updater clients (keyed by
// updater_clients.id) currently hold an open GET /v2/client/ws connection,
// in memory only - HTTP- and DB-free, like internal/statshub, and subject to
// the same single-instance limit (see that package's doc): presence known to
// one API replica is invisible to another. internal/clientws and internal/stats are its only
// caller, for decorating GET /v2/stats/clients and the stats:clients WS
// channel with an "online" field (2026-09-17-client-presence-ws-api-design.md
// §5).
package presencehub

import (
	"sync"
	"time"
)

// DefaultGraceDuration is how long a client stays "online" after its
// connection drops before it is actually marked offline (design doc §5): it
// absorbs a brief reconnect - a network blip, or a newer connection
// superseding an older one - without a flicker on the dashboard.
const DefaultGraceDuration = 15 * time.Second

// stopCategoryMachine is clientproto.StopCategoryMachine (shutdown, reboot),
// spelled here so this package stays free of the wire types.
const stopCategoryMachine = "machine"

// Token identifies one Connect call. Its matching Disconnect only acts on
// the entry it created: a superseded connection's own (deferred) Disconnect
// must be a no-op on the newer connection that replaced it, not undo its
// registration.
type Token struct {
	clientID int64
	gen      uint64
}

type entry struct {
	gen        uint64
	supersede  chan struct{}
	graceTimer *time.Timer
	// stop is set by Stopping: the connection announced it is going away
	// (service.stopped) before it dropped.
	stop *Stop
}

// Stop is why a client went offline, as its last service.stopped said
// (CLIENT_WS_PROTOCOL.md §8.7). Reason and Category are kept verbatim, so a
// value this server does not know is still reported. At is zero when the
// payload carried none, or one that did not parse.
type Stop struct {
	Reason   string
	Category string
	At       time.Time
}

// Hub tracks online presence per updater_clients.id. The zero value is not
// usable; construct with New. Safe for concurrent use.
type Hub struct {
	mu      sync.Mutex
	grace   time.Duration
	entries map[int64]*entry
	// stopped holds the announced Stop of clients whose connection ended
	// after a service.stopped, until they connect again. One entry per
	// machine at most, so it is bounded by the fleet.
	stopped map[int64]Stop
	nextGen uint64
}

// New returns an empty, ready-to-use Hub. graceDuration is normally
// DefaultGraceDuration; tests pass a shorter one so they don't have to sleep
// for 15 real seconds.
func New(graceDuration time.Duration) *Hub {
	return &Hub{grace: graceDuration, entries: make(map[int64]*entry), stopped: make(map[int64]Stop)}
}

// Connect registers clientID as online for a new connection. It returns a
// Token (pass to Disconnect when this connection ends) and a channel that
// closes if a *newer* Connect call for the same clientID supersedes this
// one - the caller's connection loop should select on it and close its own
// socket (design doc §5: "sostituisce quella vecchia... la connessione
// precedente viene chiusa lato server"). Connecting also cancels any pending
// grace-period removal left over from a previous Disconnect for this
// clientID, so a reconnect inside the grace window is seamless.
//
// A nil *Hub returns a zero-value Token and a nil channel, mirroring Online's
// nil-receiver safety: a caller that never wired presence tracking (tests, a
// build that never constructs one) can still call Connect/Disconnect without
// special-casing nil. A nil channel in a select simply never fires - it
// blocks forever, which is correct here: there is nothing to supersede a
// connection that was never tracked.
func (h *Hub) Connect(clientID int64) (Token, <-chan struct{}) {
	if h == nil {
		return Token{}, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if old, ok := h.entries[clientID]; ok {
		if old.graceTimer != nil {
			old.graceTimer.Stop()
		}
		close(old.supersede)
	}
	delete(h.stopped, clientID)

	h.nextGen++
	gen := h.nextGen
	e := &entry{gen: gen, supersede: make(chan struct{})}
	h.entries[clientID] = e
	return Token{clientID: clientID, gen: gen}, e.supersede
}

// Disconnect ends one Connect'd connection. Rather than marking the client
// offline immediately, it arms a grace-period timer that removes the entry
// when it fires; a fresh Connect for the same clientID before then cancels
// it. A Disconnect whose Token no longer matches the current entry (the
// connection was superseded - see Connect) is a no-op, so cleanup from the
// old connection never touches the new one's state.
//
// A connection that announced a stop of category "machine" (the PC shutting
// down or rebooting, see Stopping) goes offline at once instead: the grace
// window is there to absorb a reconnect, and a machine that is switching
// off will not reconnect within it. Every other announced stop - a restart
// of the service alone, a self-update - keeps the grace window, so it does
// not flicker offline.
//
// A nil *Hub, or a zero-value Token (what a nil Hub's own Connect returns),
// is a no-op - same nil-safety rationale as Connect above.
func (h *Hub) Disconnect(tok Token) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	e, ok := h.entries[tok.clientID]
	if !ok || e.gen != tok.gen {
		return
	}

	clientID, gen := tok.clientID, tok.gen
	if e.stop != nil && e.stop.Category == stopCategoryMachine {
		delete(h.entries, clientID)
		h.stopped[clientID] = *e.stop
		return
	}
	e.graceTimer = time.AfterFunc(h.grace, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if cur, ok := h.entries[clientID]; ok && cur.gen == gen {
			delete(h.entries, clientID)
			if cur.stop != nil {
				h.stopped[clientID] = *cur.stop
			}
		}
	})
}

// Online reports whether clientID currently has an open connection, or is
// still inside the grace window after one dropped. A nil Hub reports every
// client offline, so a caller that never wired presence tracking (tests, a
// build that never constructs one) doesn't need to special-case it.
func (h *Hub) Online(clientID int64) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.entries[clientID]
	return ok
}

// Stopping records that the connection behind tok announced its own end
// (service.stopped), so that once it drops and the grace period runs out
// Stopped reports why the client is offline. A later stop on the same
// connection replaces the earlier one. The grace period itself is left
// alone - a "restart" is back within it, and should not flicker offline any
// more than a network blip does - except for a "machine" stop, which skips
// it (see Disconnect). A token that no longer matches the current
// entry (superseded), a zero Token or a nil Hub is a no-op.
func (h *Hub) Stopping(tok Token, s Stop) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.entries[tok.clientID]; ok && e.gen == tok.gen {
		e.stop = &s
	}
}

// Stopped returns the announced Stop of an offline client whose last
// connection ended after a service.stopped. It reports false while the
// client is online (grace window included), once it has connected again,
// for a connection that dropped without announcing anything, and for every
// client after an API restart - this is the reason for the current offline
// spell, not a history (that is updater_clients.last_stop_*).
func (h *Hub) Stopped(clientID int64) (Stop, bool) {
	if h == nil {
		return Stop{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, online := h.entries[clientID]; online {
		return Stop{}, false
	}
	s, ok := h.stopped[clientID]
	return s, ok
}
