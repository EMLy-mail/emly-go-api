package productreg

import (
	"testing"

	"emly-api-go/internal/remoteconfig"
)

// The remote-config document's products section keys its entries by product
// slug, and internal/remoteconfig duplicates this package's slug rule instead
// of importing it (it has no feature-package dependency on purpose). This pins
// the two equal: every slug the registry would accept must be accepted by the
// document, and every slug the registry rejects or reserves must be rejected
// there too. The one deliberate difference is "emly": a registry product, but
// built into the agent, so the document forbids listing it.
func TestRemoteConfigProductSlugRuleMatchesRegistry(t *testing.T) {
	candidates := []string{
		"emly", "foo", "a", "foo-bar", "x1", "abcdefghijklmnopqrst", "3g-rocketchat",
		"", "-foo", "Foo", "foo_bar", "abcdefghijklmnopqrstu", "foo bar", "..",
	}
	for s := range reserved {
		candidates = append(candidates, s)
	}
	for _, slug := range candidates {
		registryOK := ValidSlug(slug) && !Reserved(slug) && slug != EMLy
		doc := `{"schemaVersion":1,"servers":{"a":"https://a.example.test"},"defaultServer":"a",` +
			`"products":{"` + slug + `":{"name":"X","installDir":"C:\\X","exeName":"x.exe",` +
			`"detect":[{"type":"file","path":"v.txt"}],"installer":{"type":"nsis"}}}}`
		_, problems := remoteconfig.Parse([]byte(doc))
		documentOK := true
		for _, p := range problems {
			if p.Path == "/products/"+slug {
				documentOK = false
			}
		}
		if documentOK != registryOK {
			t.Errorf("slug %q: document accepts = %v, registry (minus emly) accepts = %v; problems %+v",
				slug, documentOK, registryOK, problems)
		}
	}
}
