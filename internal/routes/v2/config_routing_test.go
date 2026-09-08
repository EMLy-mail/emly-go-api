package v2

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestConfigRoutesAuthGating pins down the auth split for /v2/config: the
// public document is API-key gated (same as the updater manifest), every
// admin route is admin-key gated, and none of it 404s for lack of a DB -
// nil db and nil configMirror are enough to prove routing/auth alone.
func TestConfigRoutesAuthGating(t *testing.T) {
	router := NewRouter(nil, nil, nil, nil, nil)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{name: "GET /config requires an API key", method: http.MethodGet, path: "/config", want: http.StatusUnauthorized},
		{name: "POST /config/validate requires the admin key", method: http.MethodPost, path: "/config/validate", want: http.StatusUnauthorized},
		{name: "POST /config/preview requires the admin key", method: http.MethodPost, path: "/config/preview", want: http.StatusUnauthorized},
		{name: "GET /config/revisions requires the admin key", method: http.MethodGet, path: "/config/revisions", want: http.StatusUnauthorized},
		{name: "POST /config/revisions requires the admin key", method: http.MethodPost, path: "/config/revisions", want: http.StatusUnauthorized},
		{name: "GET /config/revisions/{revision} requires the admin key", method: http.MethodGet, path: "/config/revisions/1", want: http.StatusUnauthorized},
		{name: "DELETE /config/revisions/{revision} requires the admin key", method: http.MethodDelete, path: "/config/revisions/1", want: http.StatusUnauthorized},
		{name: "POST /config/revisions/{revision}/publish requires the admin key", method: http.MethodPost, path: "/config/revisions/1/publish", want: http.StatusUnauthorized},
		{name: "POST /config/rollback requires the admin key", method: http.MethodPost, path: "/config/rollback", want: http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d (body: %s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestConfigRouteIsNot404 guards the same contract detail as the updater
// manifest: an authenticated request to the public document route must never
// be answered with 404 by the router itself (API design doc §5.1) - the
// handler's own 204/200 decision, not routing, is what tells the client
// "nothing published" apart from "not implemented here".
func TestConfigRouteIsNot404(t *testing.T) {
	router := NewRouter(nil, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	req.Header.Set("X-API-Key", "test-api-key")
	rec := httptest.NewRecorder()

	// The handler dereferences the nil db once the route matches, so a
	// panic here is proof the request got past routing and auth.
	defer func() {
		if r := recover(); r != nil {
			return
		}
		if rec.Code == http.StatusNotFound {
			t.Fatalf("authenticated config request returned 404")
		}
	}()

	router.ServeHTTP(rec, req)
}

// TestConfigValidateAndPreviewDoNotNeedADatabase exercises the two admin
// routes whose "document" form of input never touches storage, so they must
// answer correctly even with a nil db.
func TestConfigValidateAndPreviewDoNotNeedADatabase(t *testing.T) {
	router := NewRouter(nil, nil, nil, nil, nil)

	t.Run("validate accepts a well-formed document", func(t *testing.T) {
		body := `{"document": {"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"a"}}`
		req := httptest.NewRequest(http.MethodPost, "/config/validate", bytes.NewBufferString(body))
		req.Header.Set("X-Admin-Key", "test-admin-key")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("validate: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("validate rejects garbage with the problem list", func(t *testing.T) {
		body := `{"document": {"schemaVersion":2}}`
		req := httptest.NewRequest(http.MethodPost, "/config/validate", bytes.NewBufferString(body))
		req.Header.Set("X-Admin-Key", "test-admin-key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("validate: got %d, want 422 (body: %s)", rec.Code, rec.Body.String())
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte(`"problems"`)) {
			t.Fatalf("validate: expected a problems list in the body, got %s", rec.Body.String())
		}
	})

	t.Run("preview with an inline document does not touch the database", func(t *testing.T) {
		body := `{"document": {"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"a"}, "host": {"hostname": "RM095"}}`
		req := httptest.NewRequest(http.MethodPost, "/config/preview", bytes.NewBufferString(body))
		req.Header.Set("X-Admin-Key", "test-admin-key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("preview: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("preview requires exactly one of revision or document", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/config/preview", bytes.NewBufferString(`{}`))
		req.Header.Set("X-Admin-Key", "test-admin-key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("preview: got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
	})
}
