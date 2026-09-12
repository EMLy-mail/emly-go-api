package v1

import (
	emlyMiddleware "emly-api-go/internal/middleware"
	"log/slog"
	"net"
	"net/http"

	"emly-api-go/internal/config"
	"emly-api-go/internal/handlers"
	"emly-api-go/internal/storage"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// SunsetDate is when /v1 stops being served. Everything under it is frozen:
// every route has a v2 equivalent, and the only differences are the prefix,
// the plural in /bug-reports (v2 uses the singular) and the rate limit on the
// admin auth group.
const SunsetDate = "2026-10-31"

// DeprecationWarning logs one warn line per request reaching a v1 route.
//
// It is deliberately per-request and not sampled: the point of this line is
// to name every client still on v1 while there is still time to move it, and
// a sampled warning would leave the rare caller - the one nobody remembers
// deploying - invisible until the sunset breaks it. It goes away with /v1
// itself, so the noise has an end date.
//
// The identity fields are the same ones the fleet telemetry keys on, so a
// hostname or HWID found here can be looked up in updater_clients directly.
func DeprecationWarning(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.WarnContext(r.Context(), "deprecated API version used",
			"api_version", "v1",
			"sunset", SunsetDate,
			"method", r.Method,
			"path", r.URL.Path,
			"ip", requestIP(r),
			"hostname", r.Header.Get("X-EMLy-Hostname"),
			"hwid", r.Header.Get("X-EMLy-HWID"),
			"user_agent", r.UserAgent(),
			"message", "v1 will be disabled after "+SunsetDate+"; migrate this client to /v2")

		next.ServeHTTP(w, r)
	})
}

// requestIP returns the caller's address without the port. The global RealIP
// middleware has already rewritten RemoteAddr from X-Forwarded-For or
// X-Real-IP by the time this runs, so there is nothing else to consult.
func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// NewRouter returns a chi.Router with all /v1 routes mounted.
func NewRouter(db *sqlx.DB, s3conn *storage.S3Connector) http.Handler {
	r := chi.NewRouter()
	dbName := config.Load().Database

	rl := emlyMiddleware.NewRateLimiter(config.Load())

	r.Use(rl.Handler)
	r.Use(DeprecationWarning)

	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Server", "emly-api-go")
			w.Header().Set("X-Powered-By", "Pure Protogen sillyness :3")
			next.ServeHTTP(w, r)
		})
	})

	r.Get("/health", handlers.Health(db))

	r.Route("/api", func(r chi.Router) {
		registerAdmin(r, db, dbName)
		registerBugReports(r, db, dbName, s3conn)
	})

	return r
}
