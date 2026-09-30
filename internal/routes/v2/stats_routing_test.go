package v2

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStatsDeleteClientRoute checks that DELETE /v2/stats/clients/{id} is
// mounted, gates on X-Admin-Key like the rest of stats/*, and rejects a
// non-numeric id before touching the database - the only part of it that can
// be exercised without one.
func TestStatsDeleteClientRoute(t *testing.T) {
	r := NewRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodDelete, "/stats/clients/42", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("DELETE /stats/clients/42 without X-Admin-Key = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/stats/clients/not-a-number", nil)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE /stats/clients/not-a-number = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}
