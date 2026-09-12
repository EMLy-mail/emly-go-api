package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// serveRouteLimit fires n requests from one address through the limiter and
// reports how many reached the route.
func serveRouteLimit(mw func(http.Handler) http.Handler, n int, prepare func(*http.Request)) int {
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // stands in for "reached the route"
	}))

	reached := 0
	for i := 0; i < n; i++ {
		r := httptest.NewRequest("GET", "/whatever", nil)
		r.RemoteAddr = "203.0.113.7:1234"
		if prepare != nil {
			prepare(r)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code == http.StatusTeapot {
			reached++
		}
	}
	return reached
}

func TestRouteLimitByIPStillLimitsOrdinaryCallers(t *testing.T) {
	mw := routeLimitByIP("dashboard-key", 3, time.Minute)

	if got := serveRouteLimit(mw, 10, nil); got != 3 {
		t.Fatalf("unlimited caller reached the route %d times, want 3", got)
	}
}

// The dashboard renders every page server-side from a single address, so its
// requests must not share a per-IP budget with anyone.
func TestRouteLimitByIPExemptsDashboardKey(t *testing.T) {
	mw := routeLimitByIP("dashboard-key", 3, time.Minute)

	got := serveRouteLimit(mw, 50, func(r *http.Request) {
		r.Header.Set("X-Dashboard-Key", "dashboard-key")
	})
	if got != 50 {
		t.Fatalf("dashboard reached the route %d times, want 50", got)
	}
}

func TestRouteLimitByIPRejectsWrongOrAbsentKey(t *testing.T) {
	cases := []struct {
		name         string
		dashboardKey string
		header       string
	}{
		// A near-miss must not buy a free pass, and an unconfigured
		// DASHBOARD_KEY must not turn an empty header into one.
		{"wrong key", "dashboard-key", "not-the-key"},
		{"empty key unconfigured", "", ""},
		{"no header", "dashboard-key", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mw := routeLimitByIP(tc.dashboardKey, 3, time.Minute)

			got := serveRouteLimit(mw, 10, func(r *http.Request) {
				if tc.header != "" {
					r.Header.Set("X-Dashboard-Key", tc.header)
				}
			})
			if got != 3 {
				t.Fatalf("caller reached the route %d times, want 3", got)
			}
		})
	}
}

// Each mount keeps its own counter, the way a separate httprate.LimitByIP per
// route group did.
func TestRouteLimitByIPCountersAreIndependent(t *testing.T) {
	a := routeLimitByIP("", 2, time.Minute)
	b := routeLimitByIP("", 2, time.Minute)

	if got := serveRouteLimit(a, 5, nil); got != 2 {
		t.Fatalf("first limiter allowed %d, want 2", got)
	}
	if got := serveRouteLimit(b, 5, nil); got != 2 {
		t.Fatalf("second limiter allowed %d, want 2", got)
	}
}
