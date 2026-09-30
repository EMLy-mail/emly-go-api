package middleware

import (
	"crypto/subtle"
	"log/slog"
	"net/http"

	"emly-api-go/internal/config"
	"emly-api-go/internal/response"
)

// DashboardKeyAuth requires the configured X-Dashboard-Key. It is meant to be
// stacked on AdminKeyAuth for routes only the dashboard's server should call,
// not to replace it. With DASHBOARD_KEY unset every request is refused: the
// route is dashboard-only, and there is no dashboard to recognise.
func DashboardKeyAuth() func(http.Handler) http.Handler {
	return dashboardKeyAuth(config.Load().DashboardKey)
}

func dashboardKeyAuth(dashboardKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-Dashboard-Key")
			if dashboardKey == "" || subtle.ConstantTimeCompare([]byte(key), []byte(dashboardKey)) != 1 {
				slog.WarnContext(r.Context(), "dashboard key auth failed",
					"url", r.URL.String(), "dashboard_key_configured", dashboardKey != "")
				response.Error(w, http.StatusUnauthorized, "unauthorized dashboard key")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
