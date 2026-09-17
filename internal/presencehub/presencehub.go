// Package presencehub tracks which EMLy Updater clients (keyed by
// updater_clients.id) currently hold an open GET /v2/client/ws connection,
// in memory only - HTTP- and DB-free, like internal/statshub, and subject to
// the same single-instance limit (see that package's doc): presence known to
// one API replica is invisible to another. internal/handlers is its only
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
}

// Hub tracks online presence per updater_clients.id. The zero value is not
// usable; construct with New. Safe for concurrent use.
type Hub struct {
	mu      sync.Mutex
	grace   time.Duration
	entries map[int64]*entry
	nextGen uint64
}

// New returns an empty, ready-to-use Hub. graceDuration is normally
// DefaultGraceDuration; tests pass a shorter one so they don't have to sleep
// for 15 real seconds.
func New(graceDuration time.Duration) *Hub {
	return &Hub{grace: graceDuration, entries: make(map[int64]*entry)}
}

// Connect registers clientID as online for a new connection. It returns a
// Token (pass to Disconnect when this connection ends) and a channel that
// closes if a *newer* Connect call for the same clientID supersedes this
// one - the caller's connection loop should select on it and close its own
// socket (design doc §5: "sostituisce quella vecchia... la connessione
// precedente viene chiusa lato server"). Connecting also cancels any pending
// grace-period removal left over from a previous Disconnect for this
// clientID, so a reconnect inside the grace window is seamless.
func (h *Hub) Connect(clientID int64) (Token, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if old, ok := h.entries[clientID]; ok {
		if old.graceTimer != nil {
			old.graceTimer.Stop()
		}
		close(old.supersede)
	}

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
func (h *Hub) Disconnect(tok Token) {
	h.mu.Lock()
	defer h.mu.Unlock()

	e, ok := h.entries[tok.clientID]
	if !ok || e.gen != tok.gen {
		return
	}

	clientID, gen := tok.clientID, tok.gen
	e.graceTimer = time.AfterFunc(h.grace, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if cur, ok := h.entries[clientID]; ok && cur.gen == gen {
			delete(h.entries, clientID)
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
