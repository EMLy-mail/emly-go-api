package eventprune

import (
	"context"
	"testing"
	"time"
)

// TestStartDisabledByNonPositiveRetention pins the EVENTS_RETENTION_DAYS=0
// escape hatch: with pruning off, Start must return without ever reaching the
// database. Passing a nil *sqlx.DB is how that is asserted - any query at all
// would panic here.
func TestStartDisabledByNonPositiveRetention(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		Start(context.Background(), nil, 0)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return with pruning disabled")
	}
}

// TestStartDisabledByNegativeRetention covers the same guard for a retention
// that came out negative, which a misconfigured EVENTS_RETENTION_DAYS can
// produce. It must read as "off", never as "everything is expired".
func TestStartDisabledByNegativeRetention(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		Start(context.Background(), nil, -24*time.Hour)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return with a negative retention")
	}
}
