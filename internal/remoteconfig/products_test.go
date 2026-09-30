package remoteconfig

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Edge cases the shared fixtures (testdata/remoteconfig/*/products*.json)
// do not cover. The fixtures are the cross-repo contract; these only pin
// this side's reading of the same rules (emly-updater
// internal/policy/products.go).

// productDoc builds a minimal document with one product whose JSON is
// productJSON, so each case below states only what it is about.
func productDoc(slug, productJSON string) []byte {
	return []byte(`{"schemaVersion":1,"servers":{"a":"https://a.example.test"},"defaultServer":"a",` +
		`"products":{"` + slug + `":` + productJSON + `}}`)
}

const goodProduct = `{"enabled":true,"name":"RC","installDir":"C:\\3gIT\\RC","exeName":"RC.exe",` +
	`"detect":[{"type":"file","path":"version.txt"}],"installer":{"type":"inno"}}`

// withFields replaces top-level keys of goodProduct (key, value, key, value...).
func withFields(kv ...string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(goodProduct), &m); err != nil {
		panic(err)
	}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = json.RawMessage(kv[i+1])
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestParse_ProductsValid(t *testing.T) {
	cases := []struct{ name, slug, product string }{
		{"minimal", "rc", goodProduct},
		{"20-char slug", "abcdefghijklmnopqrst", goodProduct},
		{"upper-case .EXE, beta, 5 detect sources", "rc", withFields(
			"exeName", `"RC.EXE"`,
			"channel", `"beta"`,
			"detect", `[{"type":"file","path":"a"},{"type":"file","path":"b"},{"type":"exe","path":"c.exe"},`+
				`{"type":"ini","path":"d\\e.ini","section":"s","key":"k"},{"type":"file","path":"f/g"}]`,
		)},
		{"name of exactly 64 after trimming", "rc", withFields("name", `"  `+strings.Repeat("x", 64)+`  "`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, problems := Parse(productDoc(c.slug, c.product)); len(problems) != 0 {
				t.Fatalf("problems = %+v", problems)
			}
		})
	}
}

func TestParse_ProductsRules(t *testing.T) {
	cases := []struct {
		name, slug, product, path string
	}{
		{"slug too long", "abcdefghijklmnopqrstu", goodProduct, "/products/abcdefghijklmnopqrstu"},
		{"slug leading dash", "-rc", goodProduct, "/products/-rc"},
		{"reserved emly", "emly", goodProduct, "/products/emly"},
		{"reserved download", "download", goodProduct, "/products/download"},
		{"blank name", "rc", withFields("name", `"   "`), "/products/rc/name"},
		{"name too long", "rc", withFields("name", `"`+strings.Repeat("x", 65)+`"`), "/products/rc/name"},
		{"installDir dotdot", "rc", withFields("installDir", `"C:\\3gIT\\..\\Windows"`), "/products/rc/installDir"},
		{"installDir forward slash", "rc", withFields("installDir", `"C:/3gIT"`), "/products/rc/installDir"},
		{"exeName empty", "rc", withFields("exeName", `""`), "/products/rc/exeName"},
		{"exeName colon", "rc", withFields("exeName", `"C:RC.exe"`), "/products/rc/exeName"},
		{"exeName slash", "rc", withFields("exeName", `"bin/RC.exe"`), "/products/rc/exeName"},
		{"exeName not exe", "rc", withFields("exeName", `"RC.msi"`), "/products/rc/exeName"},
		{"detect empty", "rc", withFields("detect", `[]`), "/products/rc/detect"},
		{"detect six", "rc", withFields("detect", `[{"type":"file","path":"a"},{"type":"file","path":"a"},{"type":"file","path":"a"},`+
			`{"type":"file","path":"a"},{"type":"file","path":"a"},{"type":"file","path":"a"}]`), "/products/rc/detect"},
		{"detect path empty", "rc", withFields("detect", `[{"type":"file","path":""}]`), "/products/rc/detect/0/path"},
		{"detect path backslash-rooted", "rc", withFields("detect", `[{"type":"file","path":"\\v.txt"}]`), "/products/rc/detect/0/path"},
		{"detect path slash-rooted", "rc", withFields("detect", `[{"type":"file","path":"/v.txt"}]`), "/products/rc/detect/0/path"},
		{"ini without section", "rc", withFields("detect", `[{"type":"ini","path":"c.ini","key":"k"}]`), "/products/rc/detect/0/section"},
		{"installer missing", "rc", withFields("installer", `{}`), "/products/rc/installer/type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, problems := Parse(productDoc(c.slug, c.product))
			if !hasPath(problems, c.path) {
				t.Fatalf("want a problem at %s, got %+v", c.path, problems)
			}
		})
	}
}

// A reserved or malformed slug reports only the slug: its fields are not
// validated (same as the agent, which `continue`s).
func TestParse_ProductsBadSlugStopsThere(t *testing.T) {
	_, problems := Parse(productDoc("updater", `{}`))
	if len(problems) != 1 || problems[0].Path != "/products/updater" {
		t.Fatalf("problems = %+v, want exactly /products/updater", problems)
	}
}

func TestParse_ProductsPatchable(t *testing.T) {
	doc := `{"schemaVersion":1,"servers":{"a":"https://a.example.test"},"defaultServer":"a",` +
		`"products":{"rc":` + goodProduct + `},` +
		`"overrides":[{"id":"off","match":{"hostnames":["PC1"]},"patch":{"products":{"rc":{"enabled":false}}}}]}`
	d, problems := Parse([]byte(doc))
	if len(problems) != 0 {
		t.Fatalf("problems = %+v", problems)
	}
	eff, ids := Effective(d, Host{Hostname: "PC1"})
	if len(ids) != 1 || eff.Products["rc"].Enabled {
		t.Fatalf("ids = %v, rc.enabled = %v; want [off], false", ids, eff.Products["rc"].Enabled)
	}
	if !d.Products["rc"].Enabled {
		t.Fatal("Effective mutated the global document")
	}
}

// A patch that adds a product the global document lacks must carry a
// complete entry, otherwise the dry-run rejects it.
func TestParse_ProductsPatchAddingIncompleteProductRejected(t *testing.T) {
	doc := `{"schemaVersion":1,"servers":{"a":"https://a.example.test"},"defaultServer":"a",` +
		`"overrides":[{"id":"add","match":{"all":true},"patch":{"products":{"rc":{"enabled":true}}}}]}`
	_, problems := Parse([]byte(doc))
	if !hasPath(problems, "/overrides/0/patch/products/rc/name") {
		t.Fatalf("problems = %+v, want one at /overrides/0/patch/products/rc/name", problems)
	}
}

// Same ETag argument as TestCanonical_OmitsNilClientWS: a document without
// products must canonicalize exactly as it did before the field existed.
func TestCanonical_OmitsNilProducts(t *testing.T) {
	d, problems := Parse([]byte(`{"schemaVersion":1,"servers":{"a":"https://a.example.test"},"defaultServer":"a"}`))
	if len(problems) != 0 {
		t.Fatalf("problems = %+v", problems)
	}
	b, _ := Canonical(d)
	if bytes.Contains(b, []byte(`"products"`)) {
		t.Fatalf("canonical form gained a products key: %s", b)
	}
}

func TestUnknownFieldPaths_WalksProducts(t *testing.T) {
	unknown, err := UnknownFieldPaths(productDoc("rc", withFields("instalDir", `"C:\\x"`)))
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 1 || unknown[0] != "/products/rc/instalDir" {
		t.Fatalf("unknown = %v, want [/products/rc/instalDir]", unknown)
	}
}
