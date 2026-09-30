package updates

import (
	"testing"

	"emly-api-go/internal/models"
)

// EMLy's manifest must keep handing out the product-less download link every
// EMLy client in the field already follows; every other product links to its
// own /v2/updates/{product}/... route.
func TestBuildManifestDownloadLinks(t *testing.T) {
	releases := []models.Release{
		{Version: "2.0.0", IsBeta: true, SeverityType: "none"},
		{Version: "1.0.0", IsStable: true, SeverityType: "none"},
	}

	emly := buildManifest(releases, "https://api.example", "emly")
	if emly.StableDownload != "https://api.example/v2/updates/releases/1.0.0/download" {
		t.Errorf("emly stable link = %q", emly.StableDownload)
	}
	if emly.BetaDownload != "https://api.example/v2/updates/releases/2.0.0/download" {
		t.Errorf("emly beta link = %q", emly.BetaDownload)
	}

	foo := buildManifest(releases, "https://api.example", "foo")
	if foo.StableDownload != "https://api.example/v2/updates/foo/releases/1.0.0/download" {
		t.Errorf("foo stable link = %q", foo.StableDownload)
	}
	if foo.BetaDownload != "https://api.example/v2/updates/foo/releases/2.0.0/download" {
		t.Errorf("foo beta link = %q", foo.BetaDownload)
	}
	if foo.StableVersion != "1.0.0" || foo.BetaVersion != "2.0.0" {
		t.Errorf("foo versions = %q/%q", foo.StableVersion, foo.BetaVersion)
	}
}
