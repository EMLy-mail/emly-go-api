// internal/routes/v2/client_routing_test.go
package v2

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClientWSRouteRequiresAPIKey checks that GET /v2/client/ws is mounted
// (not 404) and gates on the API key before attempting any upgrade, via
// apimw.APIKeyAuth like the updater's self-update manifest - unlike
// /v2/stats/stream, there is no query-string key fallback here.
func TestClientWSRouteRequiresAPIKey(t *testing.T) {
	router := NewRouter(nil, nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/client/ws", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatalf("GET /client/ws returned 404; route not mounted")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /client/ws without a key = %d, want %d (body: %s)", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}
