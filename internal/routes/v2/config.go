package v2

import (
	"emly-api-go/internal/handlers"

	"github.com/go-chi/chi/v5"
)

// registerConfig mounts the public client-config route. It carries no auth and
// no extra per-route rate limit: the payload is non-sensitive and its key names
// are obfuscated daily (see internal/obfuscate).
func registerConfig(r chi.Router) {
	r.Get("/config", handlers.GetClientConfig())
}
