package v2

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProductRoutesResolve pins down the multi-product updates surface next
// to the legacy EMLy routes it must not shadow. With a nil registry only emly
// exists, so an unknown slug is a 404 and emly resolves on both route shapes.
func TestProductRoutesResolve(t *testing.T) {
	router := NewRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	admin := map[string]string{"X-Admin-Key": "test-admin-key"}
	scoped := map[string]string{"X-Admin-Key": "test-admin-key", "X-Session-Token": "tok"}

	cases := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		want    int
	}{
		{"legacy emly download still resolves", http.MethodGet, "/updates/releases/1.0.0/download", nil, http.StatusServiceUnavailable},
		{"per-product emly download resolves", http.MethodGet, "/updates/emly/releases/1.0.0/download", nil, http.StatusServiceUnavailable},
		{"unknown product manifest is 404", http.MethodGet, "/updates/foo/manifest", nil, http.StatusNotFound},
		{"unknown product download is 404", http.MethodGet, "/updates/foo/releases/1.0.0/download", nil, http.StatusNotFound},
		{"product release list needs the admin key", http.MethodGet, "/updates/foo/releases", nil, http.StatusUnauthorized},
		{"product release list: unknown product after auth", http.MethodGet, "/updates/foo/releases", admin, http.StatusNotFound},
		{"product release create guards nil S3", http.MethodPost, "/updates/emly/releases", admin, http.StatusServiceUnavailable},
		{"legacy release create guards nil S3", http.MethodPost, "/updates/releases", admin, http.StatusServiceUnavailable},
		{"updater routes are not taken for a product", http.MethodGet, "/updates/updater/releases", nil, http.StatusUnauthorized},
		{"products list needs the admin key", http.MethodGet, "/products", nil, http.StatusUnauthorized},
		{"products create needs the admin key", http.MethodPost, "/products", nil, http.StatusUnauthorized},
		{"products delete needs the admin key", http.MethodDelete, "/products/foo", nil, http.StatusUnauthorized},
		// With no database a session token resolves to no products: the
		// scope must shut, never fall back to unrestricted.
		{"unassigned product release list is 403", http.MethodGet, "/updates/emly/releases", scoped, http.StatusForbidden},
		{"unassigned legacy release list is 403", http.MethodGet, "/updates/releases", scoped, http.StatusForbidden},
		{"unassigned product is hidden from /products/{slug}", http.MethodGet, "/products/emly", scoped, http.StatusNotFound},
		{"unassigned stats product is 403", http.MethodGet, "/stats/summary?product=emly", scoped, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, nil)
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("%s %s = %d, want %d (body %s)", c.method, c.path, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}
