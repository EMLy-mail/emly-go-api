package handlers

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTTLCacheServesMemoizedValueWithinTTL(t *testing.T) {
	c := newTTLCache[int](time.Minute)
	var builds atomic.Int32

	build := func() (int, error) {
		return int(builds.Add(1)), nil
	}

	for range 5 {
		v, err := c.get("k", build)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if v != 1 {
			t.Fatalf("got value %d, want the first build", v)
		}
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("built %d times, want 1", got)
	}
}

func TestTTLCacheKeysAreIndependent(t *testing.T) {
	c := newTTLCache[int](time.Minute)
	var builds atomic.Int32

	build := func() (int, error) { return int(builds.Add(1)), nil }

	// product|window_minutes pairs: a different window must not be answered
	// from another window's cached counts.
	for _, k := range []string{"emly|15", "emly|60", "updater|15"} {
		if _, err := c.get(k, build); err != nil {
			t.Fatalf("get %s: %v", k, err)
		}
	}
	if got := builds.Load(); got != 3 {
		t.Fatalf("built %d times, want one per key", got)
	}
}

// A cached zero value must still count as cached: expiry is tracked by
// timestamp, not by the value looking empty. A fleet with no clients yet
// returns a zero-valued StatsSummary, and that must not rebuild every call.
func TestTTLCacheMemoizesZeroValue(t *testing.T) {
	c := newTTLCache[StatsSummary](time.Minute)
	var builds atomic.Int32

	build := func() (StatsSummary, error) {
		builds.Add(1)
		return StatsSummary{}, nil
	}

	for range 3 {
		if _, err := c.get("k", build); err != nil {
			t.Fatalf("get: %v", err)
		}
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("built %d times, want 1", got)
	}
}

func TestTTLCacheRebuildsAfterExpiry(t *testing.T) {
	c := newTTLCache[int](10 * time.Millisecond)
	var builds atomic.Int32

	build := func() (int, error) { return int(builds.Add(1)), nil }

	if _, err := c.get("k", build); err != nil {
		t.Fatalf("get: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := c.get("k", build); err != nil {
		t.Fatalf("get after expiry: %v", err)
	}
	if got := builds.Load(); got != 2 {
		t.Fatalf("built %d times, want 2", got)
	}
}

// A stampede of concurrent callers on a cold key is the case that matters:
// it is exactly what a polled dashboard produces, and the point of the cache
// is that they collapse into one query instead of N.
func TestTTLCacheCollapsesConcurrentCallers(t *testing.T) {
	c := newTTLCache[int](time.Minute)
	var builds atomic.Int32

	build := func() (int, error) {
		builds.Add(1)
		time.Sleep(20 * time.Millisecond)
		return 1, nil
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.get("k", build); err != nil {
				t.Errorf("get: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := builds.Load(); got != 1 {
		t.Fatalf("built %d times, want 1", got)
	}
}

func TestTTLCacheDoesNotCacheFailures(t *testing.T) {
	c := newTTLCache[int](time.Minute)
	sentinel := errors.New("boom")
	var builds atomic.Int32

	failing := func() (int, error) {
		builds.Add(1)
		return 0, sentinel
	}

	if _, err := c.get("k", failing); !errors.Is(err, sentinel) {
		t.Fatalf("got error %v, want the build error", err)
	}
	if _, err := c.get("k", failing); !errors.Is(err, sentinel) {
		t.Fatalf("second get: got error %v, want the build error", err)
	}
	if got := builds.Load(); got != 2 {
		t.Fatalf("built %d times, want a retry after failure", got)
	}
}

func TestTTLCacheDisabledByNonPositiveTTL(t *testing.T) {
	c := newTTLCache[int](0)
	var builds atomic.Int32

	build := func() (int, error) { return int(builds.Add(1)), nil }

	for range 3 {
		if _, err := c.get("k", build); err != nil {
			t.Fatalf("get: %v", err)
		}
	}
	if got := builds.Load(); got != 3 {
		t.Fatalf("built %d times, want every call to build", got)
	}
}

func TestTTLCacheBoundsKeySpace(t *testing.T) {
	c := newTTLCache[int](time.Minute)
	build := func() (int, error) { return 0, nil }

	for i := range maxTTLCacheEntries * 2 {
		if _, err := c.get(strconv.Itoa(i)+"|k", build); err != nil {
			t.Fatalf("get: %v", err)
		}
	}

	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > maxTTLCacheEntries {
		t.Fatalf("cache holds %d entries, want at most %d", n, maxTTLCacheEntries)
	}
}
