package downloadqueue

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func ptr[T any](v T) *T { return &v }

func TestNewNormalizesOutOfRangeSettings(t *testing.T) {
	q := New(Settings{Enabled: true, Capacity: 0, RetryAfter: 0})
	st := q.Status()
	if st.Capacity != DefaultCapacity || st.RetryAfterSeconds != 60 {
		t.Fatalf("got capacity %d retry %d, want %d / 60", st.Capacity, st.RetryAfterSeconds, DefaultCapacity)
	}
	if st.Defaults != st.SettingsView {
		t.Fatalf("defaults %+v differ from current %+v", st.Defaults, st.SettingsView)
	}
}

func TestTryAcquireRefusesWhenFullAndReleaseFrees(t *testing.T) {
	q := New(Settings{Enabled: true, Capacity: 2, RetryAfter: time.Minute})

	a, ok := q.TryAcquire(SlotInfo{Product: "emly"}, nil)
	if !ok {
		t.Fatal("first acquire refused")
	}
	if _, ok := q.TryAcquire(SlotInfo{Product: "emly"}, nil); !ok {
		t.Fatal("second acquire refused")
	}
	if _, ok := q.TryAcquire(SlotInfo{Product: "emly"}, nil); ok {
		t.Fatal("third acquire accepted past capacity")
	}

	q.Release(a)
	q.Release(a) // double release is a no-op
	if _, ok := q.TryAcquire(SlotInfo{Product: "emly"}, nil); !ok {
		t.Fatal("acquire after release refused")
	}

	st := q.Status()
	if st.Active != 2 || st.Available != 0 || st.AcquiredTotal != 3 || st.RejectedTotal != 1 {
		t.Fatalf("unexpected status %+v", st)
	}
}

func TestDisabledQueueTracksButNeverRefuses(t *testing.T) {
	q := New(Settings{Enabled: false, Capacity: 1, RetryAfter: time.Minute})
	for i := 0; i < 5; i++ {
		if _, ok := q.TryAcquire(SlotInfo{}, nil); !ok {
			t.Fatalf("disabled queue refused acquire %d", i)
		}
	}
	if st := q.Status(); st.Active != 5 || st.Available != 0 {
		t.Fatalf("got active %d available %d, want 5 / 0", st.Active, st.Available)
	}

	// Switching it on with 5 in flight evicts nobody, but refuses the next.
	if _, err := q.Update(Patch{Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := q.TryAcquire(SlotInfo{}, nil); ok {
		t.Fatal("acquire accepted while over capacity")
	}
	if q.Status().Active != 5 {
		t.Fatal("enabling the queue dropped slots in flight")
	}
}

func TestEvictCancelsWithErrEvicted(t *testing.T) {
	q := New(Settings{Enabled: true, Capacity: 1, RetryAfter: time.Minute})
	ctx, cancel := context.WithCancelCause(context.Background())
	id, _ := q.TryAcquire(SlotInfo{}, cancel)

	if !q.Evict(id) {
		t.Fatal("evict of a held slot reported false")
	}
	if !errors.Is(context.Cause(ctx), ErrEvicted) {
		t.Fatalf("cause = %v, want ErrEvicted", context.Cause(ctx))
	}
	if q.Evict(id) {
		t.Fatal("second evict reported true")
	}
	if st := q.Status(); st.Active != 0 || st.EvictedTotal != 1 {
		t.Fatalf("unexpected status %+v", st)
	}
}

func TestEvictAll(t *testing.T) {
	q := New(Settings{Enabled: true, Capacity: 3, RetryAfter: time.Minute})
	var ctxs []context.Context
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancelCause(context.Background())
		ctxs = append(ctxs, ctx)
		q.TryAcquire(SlotInfo{}, cancel)
	}
	if n := q.EvictAll(); n != 3 {
		t.Fatalf("evicted %d, want 3", n)
	}
	for i, ctx := range ctxs {
		if !errors.Is(context.Cause(ctx), ErrEvicted) {
			t.Fatalf("slot %d not cancelled", i)
		}
	}
	if q.Status().Active != 0 {
		t.Fatal("slots left after EvictAll")
	}
}

func TestUpdateValidatesAndResetRestoresDefaults(t *testing.T) {
	q := New(Settings{Enabled: true, Capacity: 50, RetryAfter: time.Minute})

	for _, p := range []Patch{
		{Capacity: ptr(0)},
		{Capacity: ptr(MaxCapacity + 1)},
		{RetryAfterSeconds: ptr(0)},
		{RetryAfterSeconds: ptr(86401)},
	} {
		if _, err := q.Update(p); !errors.Is(err, ErrInvalidPatch) {
			t.Fatalf("patch %+v: err = %v, want ErrInvalidPatch", p, err)
		}
	}

	st, err := q.Update(Patch{Enabled: ptr(false), Capacity: ptr(120), RetryAfterSeconds: ptr(30)})
	if err != nil {
		t.Fatal(err)
	}
	if st.Enabled || st.Capacity != 120 || st.RetryAfterSeconds != 30 || q.RetryAfterSeconds() != 30 {
		t.Fatalf("update not applied: %+v", st)
	}

	st = q.Reset()
	if !st.Enabled || st.Capacity != 50 || st.RetryAfterSeconds != 60 {
		t.Fatalf("reset did not restore defaults: %+v", st)
	}
}

func TestNilQueueIsSafe(t *testing.T) {
	var q *Queue
	if _, ok := q.TryAcquire(SlotInfo{}, nil); !ok {
		t.Fatal("nil queue refused")
	}
	q.Release(1)
	if q.Evict(1) || q.EvictAll() != 0 {
		t.Fatal("nil queue evicted something")
	}
	if _, err := q.Update(Patch{}); err == nil {
		t.Fatal("nil queue accepted an update")
	}
	if q.Status().Slots == nil {
		t.Fatal("nil queue status has nil slots")
	}

	called := false
	h := q.Middleware("emly")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Fatal("nil queue middleware did not pass through")
	}
}

// router mounts the middleware the way updates.RegisterV2 does, so the
// {version} parameter is resolved before the slot is taken.
func router(q *Queue, h http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.With(q.Middleware("emly")).Get("/releases/{version}/download", h)
	return r
}

func TestMiddlewareRefusesWith429AndRetryAfter(t *testing.T) {
	q := New(Settings{Enabled: true, Capacity: 1, RetryAfter: 90 * time.Second})

	entered := make(chan struct{})
	finish := make(chan struct{})
	r := router(q, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-finish
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/releases/1.2.3/download", nil))
	}()
	<-entered

	st := q.Status()
	if st.Active != 1 || st.Slots[0].Version != "1.2.3" || st.Slots[0].Product != "emly" {
		t.Fatalf("slot not recorded as expected: %+v", st)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/releases/1.2.3/download", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "90" {
		t.Fatalf("Retry-After = %q, want 90", got)
	}
	var body FullResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.RetryAfter != 90 || body.Capacity != 1 || body.Active != 1 || !strings.Contains(body.Message, "90 seconds") {
		t.Fatalf("unexpected body %+v", body)
	}

	close(finish)
	<-done
	if q.Status().Active != 0 {
		t.Fatal("slot not released after the download returned")
	}
}

func TestMiddlewareEvictionCancelsTheDownload(t *testing.T) {
	q := New(Settings{Enabled: true, Capacity: 1, RetryAfter: time.Minute})

	entered := make(chan struct{})
	cause := make(chan error, 1)
	r := router(q, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		cause <- context.Cause(r.Context())
	})

	go r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/releases/1.0.0/download", nil))
	<-entered

	if n := q.EvictAll(); n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
	select {
	case err := <-cause:
		if !errors.Is(err, ErrEvicted) {
			t.Fatalf("cause = %v, want ErrEvicted", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("download was not cancelled")
	}
}
