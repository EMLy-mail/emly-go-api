package middleware

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"emly-api-go/internal/timing"
)

// Timing is a middleware that measures per-request step durations.
//
// It injects a *timing.Timer into the request context so that handlers can
// record named checkpoints with timing.Mark(r.Context(), "step_name").
// After the handler returns, it logs a single line of the form:
//
//	[TIMING] METHOD /path  step1=1.2ms  step2=18ms  total=20ms
//
// Each step duration is measured from the previous checkpoint (or request
// start for the first one), so the values add up to the total.
func Timing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, t := timing.NewContext(r.Context())
		next.ServeHTTP(w, r.WithContext(ctx))

		total := time.Since(t.Start)
		checkpoints := t.Checkpoints()

		if len(checkpoints) == 0 {
			// No checkpoints: just log the total so every request is visible.
			log.Printf("[TIMING] %s %s  total=%s", r.Method, r.URL.Path, round(total))
			return
		}

		parts := make([]string, 0, len(checkpoints)+1)
		prev := t.Start
		for _, cp := range checkpoints {
			parts = append(parts, fmt.Sprintf("%s=%s", cp.Name, round(cp.At.Sub(prev))))
			prev = cp.At
		}
		// Remainder after the last checkpoint.
		if tail := total - prev.Sub(t.Start); tail > 0 {
			parts = append(parts, fmt.Sprintf("response=%s", round(tail)))
		}
		parts = append(parts, fmt.Sprintf("total=%s", round(total)))

		log.Printf("[TIMING] %s %s  %s", r.Method, r.URL.Path, strings.Join(parts, "  "))
	})
}

func round(d time.Duration) string {
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.2fµs", float64(d.Nanoseconds())/1e3)
	default:
		return fmt.Sprintf("%.2fms", float64(d.Nanoseconds())/1e6)
	}
}
