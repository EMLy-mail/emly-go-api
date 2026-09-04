package remoteconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func minimalValidDocJSON() string {
	return `{
		"schemaVersion": 1,
		"revision": 1,
		"generatedAt": "2026-09-04T10:00:00Z",
		"servers": {"srv-a": "https://a.example.com"},
		"defaultServer": "srv-a"
	}`
}

func TestParse_MinimalValidDocument(t *testing.T) {
	doc, problems := Parse([]byte(minimalValidDocJSON()))
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %+v", problems)
	}
	if doc.SchemaVersion != 1 || doc.DefaultServer != "srv-a" {
		t.Fatalf("unexpected doc: %+v", doc)
	}
}

func TestParse_SizeCap(t *testing.T) {
	huge := strings.Repeat("a", MaxDocumentBytes+1)
	_, problems := Parse([]byte(huge))
	if len(problems) != 1 || !strings.Contains(problems[0].Message, "size cap") {
		t.Fatalf("expected a size cap problem, got %+v", problems)
	}
}

func TestParse_InvalidJSON(t *testing.T) {
	_, problems := Parse([]byte(`{not json`))
	if len(problems) != 1 || !strings.Contains(problems[0].Message, "invalid JSON") {
		t.Fatalf("expected an invalid JSON problem, got %+v", problems)
	}
}

func TestParse_SchemaVersionMustBeOne(t *testing.T) {
	_, problems := Parse([]byte(`{"schemaVersion": 2, "servers": {"a": "https://a.example.com"}, "defaultServer": "a"}`))
	if !hasPath(problems, "/schemaVersion") {
		t.Fatalf("expected a /schemaVersion problem, got %+v", problems)
	}
}

func TestParse_ServersRequired(t *testing.T) {
	_, problems := Parse([]byte(`{"schemaVersion": 1}`))
	if !hasPath(problems, "/servers") {
		t.Fatalf("expected a /servers problem, got %+v", problems)
	}
	if !hasPath(problems, "/defaultServer") {
		t.Fatalf("expected a /defaultServer problem, got %+v", problems)
	}
}

func TestParse_ServerURLShape(t *testing.T) {
	cases := map[string]bool{
		"https://a.example.com":     true,
		"http://a.example.com":      true,
		"https://a.example.com/":    false, // trailing slash
		"https://a.example.com?x=1": false, // query string
		"ftp://a.example.com":       false, // wrong scheme
		"not-a-url":                 false,
	}
	for raw, wantOK := range cases {
		doc := map[string]interface{}{
			"schemaVersion": 1, "servers": map[string]string{"a": raw}, "defaultServer": "a",
		}
		b, _ := json.Marshal(doc)
		_, problems := Parse(b)
		gotOK := !hasPrefix(problems, "/servers/a")
		if gotOK != wantOK {
			t.Errorf("server URL %q: got ok=%v, want ok=%v (problems: %+v)", raw, gotOK, wantOK, problems)
		}
	}
}

func TestParse_DefaultServerMustExist(t *testing.T) {
	_, problems := Parse([]byte(`{"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"b"}`))
	if !hasPath(problems, "/defaultServer") {
		t.Fatalf("expected a /defaultServer problem, got %+v", problems)
	}
}

func TestParse_IPCProtocolDefaultVersion(t *testing.T) {
	docJSON := `{
		"schemaVersion":1,
		"servers":{"a":"https://a.example.com"},
		"defaultServer":"a",
		"ipcProtocol": {
			"versions": {"1": {"updater":{"min":null,"max":null},"emly":{"min":"2.0.0","max":null},"enabled": false}},
			"defaultVersion": 1
		}
	}`
	_, problems := Parse([]byte(docJSON))
	if !hasPath(problems, "/ipcProtocol/defaultVersion") {
		t.Fatalf("expected defaultVersion to reject a disabled version, got %+v", problems)
	}

	docJSON2 := strings.Replace(docJSON, `"defaultVersion": 1`, `"defaultVersion": 2`, 1)
	_, problems2 := Parse([]byte(docJSON2))
	if !hasPath(problems2, "/ipcProtocol/defaultVersion") {
		t.Fatalf("expected defaultVersion to reject an unknown version, got %+v", problems2)
	}
}

func TestParse_DCLookupMapRequiresSubnetAndServer(t *testing.T) {
	docJSON := `{
		"schemaVersion":1,
		"servers":{"a":"https://a.example.com"},
		"defaultServer":"a",
		"dcLookupMap": {
			"DC-1": {"internalSubnets": [], "baseServer": "unknown", "backupServer": [], "enabled": true}
		}
	}`
	_, problems := Parse([]byte(docJSON))
	if !hasPath(problems, "/dcLookupMap/DC-1/internalSubnets") {
		t.Fatalf("expected an internalSubnets problem, got %+v", problems)
	}
	if !hasPath(problems, "/dcLookupMap/DC-1/baseServer") {
		t.Fatalf("expected a baseServer problem, got %+v", problems)
	}
}

func TestParse_CIDRMustBeIPv4(t *testing.T) {
	docJSON := `{
		"schemaVersion":1,
		"servers":{"a":"https://a.example.com"},
		"defaultServer":"a",
		"dcLookupMap": {
			"DC-1": {"internalSubnets": ["2001:db8::/32"], "baseServer": "a", "backupServer": [], "enabled": true}
		}
	}`
	_, problems := Parse([]byte(docJSON))
	if !hasPath(problems, "/dcLookupMap/DC-1/internalSubnets") {
		t.Fatalf("expected an IPv6 CIDR to be rejected, got %+v", problems)
	}
}

func TestParse_OverrideMatchAllAlone(t *testing.T) {
	base := `{"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"a","overrides":[%s]}`

	// {"all": true} alone: valid.
	ov := `{"id":"o1","match":{"all":true},"patch":{"updater":{"pollIntervalMinutes":5}}}`
	_, problems := Parse([]byte(strings_Sprintf(base, ov)))
	if len(problems) != 0 {
		t.Fatalf("expected no problems for a lone all:true, got %+v", problems)
	}

	// {"all": false}: invalid.
	ov = `{"id":"o1","match":{"all":false},"patch":{}}`
	_, problems = Parse([]byte(strings_Sprintf(base, ov)))
	if !hasPath(problems, "/overrides/0/match/all") {
		t.Fatalf("expected all:false to be rejected, got %+v", problems)
	}

	// {"all": true, "hwids": [...]}: invalid, all must be alone.
	ov = `{"id":"o1","match":{"all":true,"hwids":["x"]},"patch":{}}`
	_, problems = Parse([]byte(strings_Sprintf(base, ov)))
	if !hasPath(problems, "/overrides/0/match") {
		t.Fatalf("expected all+hwids to be rejected, got %+v", problems)
	}

	// {}: invalid, empty match is not "all".
	ov = `{"id":"o1","match":{},"patch":{}}`
	_, problems = Parse([]byte(strings_Sprintf(base, ov)))
	if !hasPath(problems, "/overrides/0/match") {
		t.Fatalf("expected empty match to be rejected, got %+v", problems)
	}
}

func TestParse_ExceptRejectsAll(t *testing.T) {
	docJSON := `{
		"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"a",
		"overrides":[{"id":"o1","match":{"all":true},"except":{"all":true},"patch":{}}]
	}`
	_, problems := Parse([]byte(docJSON))
	if !hasPath(problems, "/overrides/0/except/all") {
		t.Fatalf("expected except.all to be rejected, got %+v", problems)
	}
}

func TestParse_DuplicateOverrideIDs(t *testing.T) {
	docJSON := `{
		"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"a",
		"overrides":[
			{"id":"dup","match":{"all":true},"patch":{}},
			{"id":"dup","match":{"all":true},"patch":{}}
		]
	}`
	_, problems := Parse([]byte(docJSON))
	if !hasPath(problems, "/overrides/1/id") {
		t.Fatalf("expected a duplicate id problem, got %+v", problems)
	}
}

func TestParse_PatchRestrictedToAllowedKeys(t *testing.T) {
	docJSON := `{
		"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"a",
		"overrides":[{"id":"o1","match":{"all":true},"patch":{"servers":{"a":"https://evil.example.com"}}}]
	}`
	_, problems := Parse([]byte(docJSON))
	if !hasPath(problems, "/overrides/0/patch/servers") {
		t.Fatalf("expected patch.servers to be rejected, got %+v", problems)
	}
}

func TestParse_OverrideDryRunCatchesInvalidPatchResult(t *testing.T) {
	docJSON := `{
		"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"a",
		"overrides":[{"id":"o1","match":{"all":true},"patch":{"updater":{"pollIntervalMinutes":0}}}]
	}`
	_, problems := Parse([]byte(docJSON))
	if len(problems) == 0 {
		t.Fatalf("expected the dry-run to catch pollIntervalMinutes: 0, got no problems")
	}
	found := false
	for _, p := range problems {
		if strings.Contains(p.Path, "pollIntervalMinutes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a pollIntervalMinutes problem, got %+v", problems)
	}
}

func TestParse_LoggingRanges(t *testing.T) {
	docJSON := `{
		"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"a",
		"logging": {"level":"verbose","maxSizeMB":200,"backups":100,"compress":true,"eventLog":true}
	}`
	_, problems := Parse([]byte(docJSON))
	if !hasPath(problems, "/logging/level") {
		t.Fatalf("expected a level problem, got %+v", problems)
	}
	if !hasPath(problems, "/logging/maxSizeMB") {
		t.Fatalf("expected a maxSizeMB problem, got %+v", problems)
	}
	if !hasPath(problems, "/logging/backups") {
		t.Fatalf("expected a backups problem, got %+v", problems)
	}
}

// --- shared conformance fixtures (testdata/remoteconfig, §6 of the API design doc) ---

func TestFixtures_Valid(t *testing.T) {
	dir := fixtureDir(t, "valid")
	forEachJSONFixture(t, dir, func(t *testing.T, name string, data []byte) {
		_, problems := Parse(data)
		if len(problems) != 0 {
			t.Errorf("%s: expected valid, got problems: %+v", name, problems)
		}
	})
}

func TestFixtures_Invalid(t *testing.T) {
	dir := fixtureDir(t, "invalid")
	forEachJSONFixture(t, dir, func(t *testing.T, name string, data []byte) {
		_, problems := Parse(data)
		if len(problems) == 0 {
			t.Errorf("%s: expected problems, got none", name)
			return
		}
		wantPaths := readExpectedPaths(t, filepath.Join(dir, strings.TrimSuffix(name, ".json")+".problems.json"))
		for _, want := range wantPaths {
			if !hasPath(problems, want) {
				t.Errorf("%s: expected a problem at %s, got %+v", name, want, problems)
			}
		}
	})
}

func TestFixtures_Effective(t *testing.T) {
	dir := fixtureDir(t, "effective")
	forEachJSONFixture(t, dir, func(t *testing.T, name string, data []byte) {
		var fx struct {
			Document      json.RawMessage        `json:"document"`
			Host          Host                   `json:"host"`
			ExpectedIDs   []string               `json:"expectedOverrideIds"`
			ExpectedPatch map[string]interface{} `json:"expectedEffectiveUpdaterPatch"`
		}
		if err := json.Unmarshal(data, &fx); err != nil {
			t.Fatalf("%s: %s", name, err)
		}
		doc, problems := Parse(fx.Document)
		if len(problems) != 0 {
			t.Fatalf("%s: base document invalid: %+v", name, problems)
		}
		_, ids := Effective(doc, fx.Host)
		if !equalStrings(ids, fx.ExpectedIDs) {
			t.Errorf("%s: applied override ids = %v, want %v", name, ids, fx.ExpectedIDs)
		}
	})
}

// --- helpers ---

func hasPath(problems []Problem, path string) bool {
	for _, p := range problems {
		if p.Path == path {
			return true
		}
	}
	return false
}

func hasPrefix(problems []Problem, prefix string) bool {
	for _, p := range problems {
		if strings.HasPrefix(p.Path, prefix) {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fixtureDir(t *testing.T, sub string) string {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "remoteconfig", sub)
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("fixture dir %s not present: %s", dir, err)
	}
	return dir
}

func forEachJSONFixture(t *testing.T, dir string, fn func(t *testing.T, name string, data []byte)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %s", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".problems.json") {
			continue
		}
		name := e.Name()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %s", name, err)
		}
		t.Run(name, func(t *testing.T) { fn(t, name, data) })
	}
}

func readExpectedPaths(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %s", path, err)
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		t.Fatalf("parse %s: %s", path, err)
	}
	return paths
}

func strings_Sprintf(format, arg string) string {
	return strings.Replace(format, "%s", arg, 1)
}
