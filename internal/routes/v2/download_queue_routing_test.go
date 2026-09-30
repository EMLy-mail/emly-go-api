package v2

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"emly-api-go/internal/downloadqueue"
)

// TestDownloadQueueAdminNeedsBothKeys pins the /v2/download-queue gating:
// the admin key alone is not enough, nor is the dashboard key alone.
func TestDownloadQueueAdminNeedsBothKeys(t *testing.T) {
	q := downloadqueue.New(downloadqueue.Settings{Enabled: true, Capacity: 5, RetryAfter: time.Minute})
	router := NewRouter(nil, nil, nil, nil, nil, nil, nil, nil, q)

	cases := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		want    int
	}{
		{"no keys", http.MethodGet, "/download-queue", nil, http.StatusUnauthorized},
		{"admin key only", http.MethodGet, "/download-queue", map[string]string{"X-Admin-Key": "test-admin-key"}, http.StatusUnauthorized},
		{"dashboard key only", http.MethodGet, "/download-queue", map[string]string{"X-Dashboard-Key": "test-dashboard-key"}, http.StatusUnauthorized},
		{"wrong dashboard key", http.MethodGet, "/download-queue", map[string]string{"X-Admin-Key": "test-admin-key", "X-Dashboard-Key": "nope"}, http.StatusUnauthorized},
		{"both keys", http.MethodGet, "/download-queue", map[string]string{"X-Admin-Key": "test-admin-key", "X-Dashboard-Key": "test-dashboard-key"}, http.StatusOK},
		{"evict unknown slot", http.MethodDelete, "/download-queue/slots/42", map[string]string{"X-Admin-Key": "test-admin-key", "X-Dashboard-Key": "test-dashboard-key"}, http.StatusNotFound},
		{"reset", http.MethodPost, "/download-queue/reset", map[string]string{"X-Admin-Key": "test-admin-key", "X-Dashboard-Key": "test-dashboard-key"}, http.StatusOK},
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

// TestDownloadQueueNilAnswers503 covers a router built without a queue.
func TestDownloadQueueNilAnswers503(t *testing.T) {
	router := NewRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/download-queue", nil)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	req.Header.Set("X-Dashboard-Key", "test-dashboard-key")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
}

// TestFullQueueRefusesBothInstallerDownloads checks the queue is mounted on
// both download routes, ahead of the handler: with every slot taken the
// answer is 429 rather than the nil-S3 guard's 503.
func TestFullQueueRefusesBothInstallerDownloads(t *testing.T) {
	q := downloadqueue.New(downloadqueue.Settings{Enabled: true, Capacity: 1, RetryAfter: 60 * time.Second})
	id, _ := q.TryAcquire(downloadqueue.SlotInfo{Product: "emly"}, nil)
	router := NewRouter(nil, nil, nil, nil, nil, nil, nil, nil, q)

	for _, path := range []string{"/updates/releases/1.7.0/download", "/updates/download/updater/1.5.0"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
			t.Fatalf("%s = %d Retry-After %q, want 429 / 60", path, rec.Code, rec.Header().Get("Retry-After"))
		}
	}

	q.Release(id)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/updates/download/updater/1.5.0", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("after release got %d, want the handler's 503", rec.Code)
	}
}
