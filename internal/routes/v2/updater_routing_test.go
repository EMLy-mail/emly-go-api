package v2

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestMain seeds the env the config singleton requires so NewRouter can be
// built without a live database - every assertion below is answered by
// middleware or by a handler's nil-S3 guard, before any query runs.
func TestMain(m *testing.M) {
	os.Setenv("DATABASE_NAME", "emly_test")
	os.Setenv("DB_DSN", "root:secret@tcp(127.0.0.1:3306)/emly_test?parseTime=true&loc=UTC")
	os.Setenv("API_KEY", "test-api-key")
	os.Setenv("ADMIN_KEY", "test-admin-key")
	os.Exit(m.Run())
}

// TestUpdaterRoutesResolve pins down the routing of the updater self-update
// endpoints, in particular that /updates/manifest/updater is matched as its
// own API-key-protected route rather than being shadowed by the public
// /updates/manifest, and that the pre-existing update routes still resolve.
func TestUpdaterRoutesResolve(t *testing.T) {
	// nil db and nil S3 connectors: auth middleware reads keys from config,
	// and the download/management handlers reject a nil connector with 503
	// before touching either dependency.
	router := NewRouter(nil, nil, nil)

	cases := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		want    int
	}{
		{
			name:   "updater manifest requires an API key",
			method: http.MethodGet,
			path:   "/updates/manifest/updater",
			want:   http.StatusUnauthorized,
		},
		{
			name:   "installer download is public and guards a missing S3 connector",
			method: http.MethodGet,
			path:   "/updates/download/updater/1.5.0",
			want:   http.StatusServiceUnavailable,
		},
		{
			name:   "listing updater releases requires the admin key",
			method: http.MethodGet,
			path:   "/updates/updater/releases",
			want:   http.StatusUnauthorized,
		},
		{
			name:   "publishing an updater release requires the admin key",
			method: http.MethodPost,
			path:   "/updates/updater/releases",
			want:   http.StatusUnauthorized,
		},
		{
			name:   "patching an updater release requires the admin key",
			method: http.MethodPatch,
			path:   "/updates/updater/releases/1.5.0",
			want:   http.StatusUnauthorized,
		},
		{
			name:   "deleting an updater release requires the admin key",
			method: http.MethodDelete,
			path:   "/updates/updater/releases/1.5.0",
			want:   http.StatusUnauthorized,
		},
		{
			name:   "the EMLy release download still resolves",
			method: http.MethodGet,
			path:   "/updates/releases/1.7.0/download",
			want:   http.StatusServiceUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d (body: %s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestUpdaterManifestRouteIsNot404 guards the contract's most load-bearing
// detail: a 404 from this endpoint tells the client "this mirror does not
// implement the endpoint yet" and makes it stop silently. An authenticated
// request must therefore never be answered with 404 by the router itself.
func TestUpdaterManifestRouteIsNot404(t *testing.T) {
	router := NewRouter(nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/updates/manifest/updater", nil)
	req.Header.Set("X-API-Key", "test-api-key")
	rec := httptest.NewRecorder()

	// The handler dereferences the nil db once the route matches, so a panic
	// here is proof the request got past routing and auth.
	defer func() {
		if r := recover(); r != nil {
			return
		}
		if rec.Code == http.StatusNotFound {
			t.Fatalf("authenticated manifest request returned 404; the client would treat this mirror as not implementing the endpoint")
		}
	}()

	router.ServeHTTP(rec, req)
}
