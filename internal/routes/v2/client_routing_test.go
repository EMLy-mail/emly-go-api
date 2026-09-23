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
	router := NewRouter(nil, nil, nil, nil, nil, nil, nil, nil)

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

// TestClientAdminRoutesRequireAdminKey checks that the /v2/client admin
// routes (issue/get command, list events, notify) are mounted and gate on
// X-Admin-Key like every other admin route, not the API key GET
// /v2/client/ws uses.
func TestClientAdminRoutesRequireAdminKey(t *testing.T) {
	r := NewRouter(nil, nil, nil, nil, nil, nil, nil, nil)
	for _, c := range []struct{ method, path string }{
		{"POST", "/client/42/commands"},
		{"GET", "/client/commands/01J8ZQ6T3M6X9K2V7B4N1C5D8E"},
		{"GET", "/client/42/events"},
		{"POST", "/client/notify"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without X-Admin-Key = %d, want 401", c.method, c.path, rec.Code)
		}
	}
}
