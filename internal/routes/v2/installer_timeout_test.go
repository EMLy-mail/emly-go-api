package v2

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"emly-api-go/internal/updates"

	"github.com/go-chi/chi/v5"
)

// TestInstallerDownloadPredicateMatchesRoutes keeps updates.IsInstallerDownload
// - which main.go uses, before routing, to exempt the installer downloads
// from the global 30s timeout - in step with the routes actually mounted.
// Renaming either route without updating the predicate would silently put
// it back under the 30s cut.
func TestInstallerDownloadPredicateMatchesRoutes(t *testing.T) {
	router := NewRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil)

	var downloads []string
	_ = chi.Walk(router.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet && strings.HasPrefix(route, "/updates/") && strings.Contains(route, "download") {
			downloads = append(downloads, route)
		}
		return nil
	})
	if len(downloads) != 2 {
		t.Fatalf("found download routes %v, want exactly the two installer downloads", downloads)
	}
	for _, route := range downloads {
		path := "/v2" + strings.ReplaceAll(route, "{version}", "1.2.3")
		if !updates.IsInstallerDownload(httptest.NewRequest(http.MethodGet, path, nil)) {
			t.Errorf("%s is mounted but not exempt from the global timeout", path)
		}
	}

	for _, path := range []string{
		"/v2/updates/manifest",
		"/v2/updates/releases",
		"/v2/updates/releases/1.2.3",
		"/v2/updates/releases/1.2.3/download/extra",
		"/v2/stats/summary",
	} {
		if updates.IsInstallerDownload(httptest.NewRequest(http.MethodGet, path, nil)) {
			t.Errorf("%s should stay under the global timeout", path)
		}
	}
	if updates.IsInstallerDownload(httptest.NewRequest(http.MethodDelete, "/v2/updates/releases/1.2.3/download", nil)) {
		t.Error("only GET is exempt")
	}
}
