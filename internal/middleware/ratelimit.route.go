package middleware

import (
	"net/http"
	"time"

	"emly-api-go/internal/config"

	"github.com/go-chi/httprate"
)

// RouteLimitByIP is the per-route limiter every router mounts in place of
// httprate.LimitByIP. It behaves identically except that a request carrying
// the configured X-Dashboard-Key skips the counter entirely.
//
// The exemption exists because the dashboard is a server-side renderer: every
// page it draws calls the API from one address on behalf of whichever admin is
// browsing, so a per-IP budget meant for a single caller is instead shared by
// the whole staff and empties as soon as two people click around at once. The
// global limiter in ratelimit.ban.go already exempts this key for the same
// reason; this keeps the two consistent rather than leaving the route-level
// limiter as a second, invisible ceiling.
//
// Holding the key is equivalent to holding the admin key in blast radius - it
// is a server-side secret the dashboard never ships to a browser - so no
// budget is lost by trusting it. When DASHBOARD_KEY is unset the exemption is
// off and this is plain httprate.
func RouteLimitByIP(requestLimit int, windowLength time.Duration) func(http.Handler) http.Handler {
	return routeLimitByIP(config.Load().DashboardKey, requestLimit, windowLength)
}

// routeLimitByIP is RouteLimitByIP with the key handed in rather than read
// from the process config, so the exemption can be exercised without standing
// up a whole environment.
func routeLimitByIP(dashboardKey string, requestLimit int, windowLength time.Duration) func(http.Handler) http.Handler {
	limit := httprate.LimitByIP(requestLimit, windowLength)

	return func(next http.Handler) http.Handler {
		limited := limit(next)

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if dashboardKey != "" && r.Header.Get("X-Dashboard-Key") == dashboardKey {
				next.ServeHTTP(w, r)
				return
			}
			limited.ServeHTTP(w, r)
		})
	}
}
