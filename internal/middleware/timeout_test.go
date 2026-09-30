package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTimeoutExceptSkipsOnlyWhatItIsTold(t *testing.T) {
	var hadDeadline bool
	h := TimeoutExcept(30*time.Second, func(r *http.Request) bool {
		return r.URL.Path == "/download"
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadDeadline = r.Context().Deadline()
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/other", nil))
	if !hadDeadline {
		t.Fatal("a regular request got no deadline")
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/download", nil))
	if hadDeadline {
		t.Fatal("a skipped request still got the global deadline")
	}
}
