package downloadqueue

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func ptr[T any](v T) *T { return &v }

func TestNewNormalizesOutOfRangeSettings(t *testing.T) {
	q := New(Settings{Enabled: true, Capacity: 0, RetryAfter: 0})
	st := q.Status()
	if st.Capacity != DefaultCapacity || st.RetryAfterSeconds != 60 || st.DownloadTimeoutSeconds != 600 {
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
	if st.Active != 2 || st.Available != 0 || st.CompletedTotal != 1 || st.FailedTotal != 0 || st.RejectedTotal != 1 {
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
	if st := q.Status(); st.Active != 0 || st.EvictedTotal != 1 || st.FailedTotal != 0 || st.CompletedTotal != 0 {
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
		{DownloadTimeoutSeconds: ptr(0)},
		{DownloadTimeoutSeconds: ptr(86401)},
	} {
		if _, err := q.Update(p); !errors.Is(err, ErrInvalidPatch) {
			t.Fatalf("patch %+v: err = %v, want ErrInvalidPatch", p, err)
		}
	}

	st, err := q.Update(Patch{Enabled: ptr(false), Capacity: ptr(120), RetryAfterSeconds: ptr(30), DownloadTimeoutSeconds: ptr(900)})
	if err != nil {
		t.Fatal(err)
	}
	if st.Enabled || st.Capacity != 120 || st.RetryAfterSeconds != 30 || q.RetryAfterSeconds() != 30 || st.DownloadTimeoutSeconds != 900 {
		t.Fatalf("update not applied: %+v", st)
	}

	st = q.Reset()
	if !st.Enabled || st.Capacity != 50 || st.RetryAfterSeconds != 60 || st.DownloadTimeoutSeconds != 600 {
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

func TestSlotReportsProgressAndAverageSpeed(t *testing.T) {
	q := New(Settings{Enabled: true, Capacity: 1, RetryAfter: time.Minute})
	start := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := start
	q.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }

	wrote := make(chan struct{})
	finish := make(chan struct{})
	r := router(q, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 250))
		close(wrote)
		<-finish
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/releases/1.0.0/download", nil))
	}()
	<-wrote

	mu.Lock()
	now = start.Add(5 * time.Second)
	mu.Unlock()

	st := q.Status()
	got := st.Slots[0]
	if got.BytesSent != 250 || got.BytesTotal != 1000 || got.Percent != 25 {
		t.Fatalf("progress = %d/%d (%v%%), want 250/1000 (25%%)", got.BytesSent, got.BytesTotal, got.Percent)
	}
	if got.AvgBytesPerSec != 50 || got.ElapsedSeconds != 5 || st.TotalBytesPerSec != 50 {
		t.Fatalf("speed = %d B/s over %vs (total %d), want 50 B/s over 5s", got.AvgBytesPerSec, got.ElapsedSeconds, st.TotalBytesPerSec)
	}

	close(finish)
	<-done
}

// TestMiddlewareClassifiesOutcomes pins which counter each way a download
// can end lands in. The server-timeout case is the production one: the
// global 30s deadline cancels the S3 read, io.Copy stops short without the
// handler seeing an error, and before this it was counted as a success.
func TestMiddlewareClassifiesOutcomes(t *testing.T) {
	expired := func() context.Context {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		t.Cleanup(cancel)
		return ctx
	}
	canceled := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	stream := func(sent int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(make([]byte, sent))
		}
	}

	cases := []struct {
		name    string
		ctx     func() context.Context
		handler http.HandlerFunc
		reason  string // "" = completed
	}{
		{"whole installer sent", context.Background, stream(1000), ""},
		{"server timeout mid-transfer", expired, stream(400), ReasonServerTimeout},
		{"client disconnected mid-transfer", canceled, stream(400), ReasonClientDisconnected},
		{"short copy with a live context", context.Background, stream(400), ReasonCopyFailed},
		{"release not found", context.Background, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "release not found", http.StatusNotFound)
		}, ReasonErrorResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := New(Settings{Enabled: true, Capacity: 1, RetryAfter: time.Minute})
			req := httptest.NewRequest(http.MethodGet, "/releases/1.0.0/download", nil).WithContext(tc.ctx())
			router(q, tc.handler).ServeHTTP(httptest.NewRecorder(), req)

			st := q.Status()
			if st.Active != 0 {
				t.Fatalf("slot still held after the handler returned")
			}
			if tc.reason == "" {
				if st.CompletedTotal != 1 || st.FailedTotal != 0 {
					t.Fatalf("completed %d failed %d, want 1 / 0", st.CompletedTotal, st.FailedTotal)
				}
				return
			}
			if st.CompletedTotal != 0 || st.FailedTotal != 1 || st.FailedByReason[tc.reason] != 1 {
				t.Fatalf("completed %d failed %d by reason %v, want 0 / 1 under %q",
					st.CompletedTotal, st.FailedTotal, st.FailedByReason, tc.reason)
			}
		})
	}
}

func TestMiddlewareCountsAPanicAsFailed(t *testing.T) {
	q := New(Settings{Enabled: true, Capacity: 1, RetryAfter: time.Minute})
	h := router(q, func(http.ResponseWriter, *http.Request) { panic("boom") })
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/releases/1.0.0/download", nil))
	}()
	if st := q.Status(); st.Active != 0 || st.FailedByReason[ReasonInternalError] != 1 {
		t.Fatalf("unexpected status %+v", st)
	}
}

// TestDownloadTimeoutCutsTheTransfer is the replacement for the global 30s
// timeout: the queue's own deadline cancels a download that runs past it,
// and it lands in failed_by_reason as a server timeout.
func TestDownloadTimeoutCutsTheTransfer(t *testing.T) {
	q := New(Settings{Enabled: true, Capacity: 1, RetryAfter: time.Minute, DownloadTimeout: time.Second})
	// Shorter than the 1s minimum the settings accept, so the test stays fast.
	q.current.DownloadTimeout = 50 * time.Millisecond

	h := router(q, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 100))
		<-r.Context().Done()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/releases/1.0.0/download", nil))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("download timeout did not cut the transfer")
	}
	if st := q.Status(); st.FailedByReason[ReasonServerTimeout] != 1 {
		t.Fatalf("failed_by_reason = %v, want one server timeout", st.FailedByReason)
	}
}

func TestTimeoutChangeOnlyAffectsNewDownloads(t *testing.T) {
	q := New(Settings{Enabled: true, Capacity: 2, RetryAfter: time.Minute, DownloadTimeout: 10 * time.Minute})
	start := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return start }

	first, _ := q.TryAcquire(SlotInfo{}, nil)
	if _, err := q.Update(Patch{DownloadTimeoutSeconds: ptr(60)}); err != nil {
		t.Fatal(err)
	}
	second, _ := q.TryAcquire(SlotInfo{}, nil)

	deadlines := map[uint64]time.Time{}
	for _, s := range q.Status().Slots {
		deadlines[s.ID] = s.DeadlineAt
	}
	if want := start.Add(10 * time.Minute); !deadlines[first].Equal(want) {
		t.Fatalf("in-flight deadline = %v, want %v (unchanged)", deadlines[first], want)
	}
	if want := start.Add(time.Minute); !deadlines[second].Equal(want) {
		t.Fatalf("new deadline = %v, want %v", deadlines[second], want)
	}
}

func TestNilQueueStillBoundsTheDownload(t *testing.T) {
	var q *Queue
	var deadline time.Time
	var ok bool
	h := q.Middleware("emly")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok = r.Context().Deadline()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !ok || time.Until(deadline) > DefaultDownloadTimeout || time.Until(deadline) < DefaultDownloadTimeout-time.Minute {
		t.Fatalf("deadline = %v (set %v), want ~%v from now", deadline, ok, DefaultDownloadTimeout)
	}
}
