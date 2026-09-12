package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	chiMiddleware "github.com/go-chi/chi/v5/middleware"
)

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

// dashboardUserAgent is the User-Agent prefix the EMLy Dashboard sends. The
// dashboard polls a handful of endpoints continuously (the stats summary above
// all), so at info level its traffic drowns out the fleet's own requests in
// aggregated logs. Matched case-insensitively on the prefix so a version
// suffix ("EMLy-Dashboard/1.2.0") still counts.
const dashboardUserAgent = "emly-dashboard"

// isDashboardRequest reports whether ua belongs to the EMLy Dashboard.
func isDashboardRequest(ua string) bool {
	return strings.HasPrefix(strings.ToLower(ua), dashboardUserAgent)
}

// AccessLog logs one structured line per completed request, including the
// User-Agent header so clients can be identified in aggregated logs. Dashboard
// requests are logged at debug level instead of info: they are frequent,
// self-inflicted polling, and keeping them out of the info stream leaves that
// stream about clients in the field.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w}

		next.ServeHTTP(lrw, r)

		if lrw.status == 0 {
			lrw.status = http.StatusOK
		}

		UAString := r.UserAgent()

		adDomain := r.Header.Get("X-EMLy-ADDomain")
		hostName := r.Header.Get("X-EMLy-Hostname")
		hwid := r.Header.Get("X-EMLy-HWID")
		loggedUser := r.Header.Get("X-EMLy-LoggedUser")
		serial := r.Header.Get("X-EMLy-Serial")
		product := r.Header.Get("X-EMLy-Product")

		// Log AD domain and hostname as separate fields to avoid escaping
		// characters like backslashes inside the user_agent field.
		args := []any{
			"request_id", chiMiddleware.GetReqID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", lrw.status,
			"bytes", lrw.bytes,
			"duration", time.Since(started).String(),
			"user_agent", UAString,
		}
		if adDomain != "" {
			args = append(args, "ad_domain", adDomain)
		}
		if hostName != "" {
			args = append(args, "host_name", hostName)
		}
		if hwid != "" {
			args = append(args, "hwid", hwid)
		}
		// Its own field for the same reason ad_domain has one: a Windows
		// account name embeds a backslash, and folding one into the
		// user_agent field is exactly what that split exists to avoid.
		if loggedUser != "" {
			args = append(args, "logged_user", loggedUser)
		}
		// Logged for the same reason as logged_user, and worth the two extra
		// fields: these three are the newest headers, so "is the client
		// sending them at all" is a live question, and answering it here
		// costs one API deploy instead of a rollout to the whole fleet. Each
		// stays absent when the header is, so a client too old to send them
		// adds nothing to the line.
		if serial != "" {
			args = append(args, "serial", serial)
		}
		if product != "" {
			args = append(args, "product", product)
		}
		if isDashboardRequest(UAString) {
			slog.DebugContext(r.Context(), "request", args...)
			return
		}
		slog.InfoContext(r.Context(), "request", args...)
	})
}
