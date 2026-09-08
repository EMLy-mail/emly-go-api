package v2

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStatsStreamRouteRequiresAdminKey checks that GET /v2/stats/stream is
// mounted (not 404) and gates on the admin key before attempting any
// upgrade, same as the REST stats/* routes it sits next to - unlike them, it
// checks the key itself (design doc §4) rather than through
// apimw.AdminKeyAuth, so this pins down the routing wire-up rather than
// duplicating internal/handlers' own auth-gating tests.
func TestStatsStreamRouteRequiresAdminKey(t *testing.T) {
	router := NewRouter(nil, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/stats/stream", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatalf("GET /stats/stream returned 404; route not mounted")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /stats/stream without a key = %d, want %d (body: %s)", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}
