package middleware

import (
	"net/http"
	"time"

	chiMiddleware "github.com/go-chi/chi/v5/middleware"
)

// TimeoutExcept is chi's Timeout for every request except those skip
// reports true for, which pass through with no deadline of their own.
//
// It exists for the installer downloads: a context deadline can only be
// shortened by a child, never extended, so a route that needs longer than
// the global 30s has to be kept out of it here and bound its own time
// further down (internal/downloadqueue does, with a deadline the dashboard
// can change). skip runs before routing, so it can only look at the raw
// request - method and path.
func TimeoutExcept(timeout time.Duration, skip func(*http.Request) bool) func(http.Handler) http.Handler {
	limit := chiMiddleware.Timeout(timeout)
	return func(next http.Handler) http.Handler {
		limited := limit(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			limited.ServeHTTP(w, r)
		})
	}
}
