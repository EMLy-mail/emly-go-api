package middleware

import (
	"log/slog"
	"net/http"
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

// AccessLog logs one structured line per completed request, including the
// User-Agent header so clients can be identified in aggregated logs.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w}

		next.ServeHTTP(lrw, r)

		if lrw.status == 0 {
			lrw.status = http.StatusOK
		}

		var UAString string

		adDomain := r.Header.Get("X-EMLy-ADDomain")
		hostName := r.Header.Get("X-EMLy-Hostname")

		if adDomain != "" && hostName != "" {
			UAString = r.UserAgent() + " [ + " + adDomain + "\\" + hostName + " ]"
		} else {
			UAString = r.UserAgent()
		}

		slog.InfoContext(r.Context(), "request",
			"request_id", chiMiddleware.GetReqID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", lrw.status,
			"bytes", lrw.bytes,
			"duration", time.Since(started).String(),
			"user_agent", UAString,
		)
	})
}
