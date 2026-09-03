package handlers

import (
	"encoding/json"
	"testing"

	"emly-api-go/internal/models"
)

// TestEmptyUpdaterManifestWireFormat pins the "nothing to distribute" body.
// The client reads an empty version as a silent no-op, so the zero value must
// serialize to exactly {"version":""} - no null download, no null checksum,
// nothing that could be mistaken for a real release.
func TestEmptyUpdaterManifestWireFormat(t *testing.T) {
	b, err := json.Marshal(models.UpdaterManifest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(b), `{"version":""}`; got != want {
		t.Fatalf("empty manifest = %s, want %s", got, want)
	}
}

// TestPopulatedUpdaterManifestWireFormat checks the field names the client
// parses, which are camelCase and differ from the snake_case admin payloads.
func TestPopulatedUpdaterManifestWireFormat(t *testing.T) {
	b, err := json.Marshal(models.UpdaterManifest{
		Version:      "1.5.0",
		Download:     "https://api.emly.ffois.it/v2/updates/download/updater/1.5.0",
		SHA256:       "3f786850e387550fdab836ed7e6dc881de23001b1f8b9a3d6e5c4a2b0f9e8d7c",
		Size:         4812345,
		PublishedAt:  "2026-08-28T09:00:00Z",
		ReleaseNotes: map[string]string{"it": "Note.", "en": "Notes."},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"version", "download", "sha256", "size", "publishedAt", "releaseNotes"} {
		if _, ok := got[key]; !ok {
			t.Errorf("manifest is missing %q: %s", key, b)
		}
	}
}

func TestUpdaterVersionPattern(t *testing.T) {
	valid := []string{"1.5.0", "1.5", "1.5.0.3", "2.0.0-beta.1"}
	for _, v := range valid {
		if !updaterVersionPattern.MatchString(v) {
			t.Errorf("version %q should be accepted", v)
		}
	}

	// A leading "v" would never match the installer's ApplicationVersion, so
	// the client could not tell a successful update from a failed one.
	invalid := []string{"v1.5.0", "1", "", "latest", "1.5.0 ", "../1.5.0"}
	for _, v := range invalid {
		if updaterVersionPattern.MatchString(v) {
			t.Errorf("version %q should be rejected", v)
		}
	}
}

func TestUpdaterInstallerFilename(t *testing.T) {
	cases := []struct {
		uploaded, version, want string
	}{
		{"EMLyUpdater_Installer_1.5.0.exe", "1.5.0", "EMLyUpdater_Installer_1.5.0.exe"},
		{"", "1.5.0", "EMLyUpdater_Installer_1.5.0.exe"},
		// Browsers and some clients send a full path; it must never widen the
		// S3 key beyond the configured prefix.
		{`C:\build\out\EMLyUpdater_Installer_1.5.0.exe`, "1.5.0", "EMLyUpdater_Installer_1.5.0.exe"},
		{"../../secrets.txt", "1.5.0", "secrets.txt"},
		{"a/b/c.exe", "1.5.0", "c.exe"},
	}
	for _, tc := range cases {
		if got := updaterInstallerFilename(tc.uploaded, tc.version); got != tc.want {
			t.Errorf("updaterInstallerFilename(%q, %q) = %q, want %q", tc.uploaded, tc.version, got, tc.want)
		}
	}
}
