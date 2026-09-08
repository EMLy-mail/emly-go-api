package handlers

import (
	"sync"
	"time"
)

// maxTTLCacheEntries bounds a ttlCache's key space. Keys are derived from
// query params, so a caller varying one on every request would otherwise grow
// the map without limit. Past this size the whole map is dropped rather than
// tracked with a real eviction policy: the only cost of a miss is rebuilding
// one value, and the endpoints using this cache have a handful of keys in
// practice.
const maxTTLCacheEntries = 512

// ttlCache memoizes a handler payload for a short TTL and collapses callers
// racing on the same key into a single computation.
//
// It exists because GET /v2/stats/summary serves 24h aggregates that polling
// dashboards request far faster than the numbers can meaningfully change - a
// ~200-client fleet was driving roughly one request per second, and every one
// of them re-scanned each updater_events row of the last day. One rebuild per
// TTL is indistinguishable to a human reading the page and takes the endpoint
// off the database's hot path.
//
// Only the polled REST path is cached. The WS stream (stats_stream.route.go)
// calls fetchStatsSummary directly, because it recomputes on an actual event
// or tick rather than on client demand - putting it behind this cache would
// buy nothing and could push a stale snapshot to a subscriber.
type ttlCache[T any] struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]*ttlEntry[T]
}

type ttlEntry[T any] struct {
	// mu is held for the whole rebuild, so callers arriving on a cold or
	// stale key wait for the one in-flight computation instead of each
	// issuing their own.
	mu        sync.Mutex
	value     T
	expiresAt time.Time
}

func newTTLCache[T any](ttl time.Duration) *ttlCache[T] {
	return &ttlCache[T]{ttl: ttl, entries: make(map[string]*ttlEntry[T])}
}

// get returns the value cached under key, calling build to recompute it when
// the entry is missing or expired. The returned value is shared by every
// caller holding that key and must be treated as read-only.
//
// A build error reaches every caller waiting on the key and caches nothing,
// so the next request retries. That includes the case where the request
// driving the rebuild is cancelled mid-flight: build runs on that request's
// context, and its waiters fail with it rather than inheriting a detached
// query with no deadline.
//
// A non-positive TTL disables caching entirely and every call builds - the
// escape hatch for reading live numbers while debugging (STATS_CACHE_TTL=0).
func (c *ttlCache[T]) get(key string, build func() (T, error)) (T, error) {
	if c.ttl <= 0 {
		return build()
	}

	c.mu.Lock()
	if len(c.entries) >= maxTTLCacheEntries {
		c.entries = make(map[string]*ttlEntry[T])
	}
	e, ok := c.entries[key]
	if !ok {
		e = &ttlEntry[T]{}
		c.entries[key] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	// A zero expiresAt marks an entry that has never been built, which is
	// what distinguishes "cold" from "holds a valid zero-valued T".
	if !e.expiresAt.IsZero() && time.Now().Before(e.expiresAt) {
		return e.value, nil
	}

	value, err := build()
	if err != nil {
		var zero T
		return zero, err
	}
	e.value = value
	e.expiresAt = time.Now().Add(c.ttl)
	return value, nil
}
