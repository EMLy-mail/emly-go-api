package productreg

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"emly-api-go/internal/models"
)

func strPtr(s string) *string { return &s }

func TestValidSlug(t *testing.T) {
	for _, s := range []string{"emly", "foo", "a", "foo-bar", "x1", "abcdefghijklmnopqrst"} {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "-foo", "Foo", "foo_bar", "foo/bar", "abcdefghijklmnopqrstu", "foo bar", ".."} {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true, want false", s)
		}
	}
}

// Every static segment under /v2/updates must be reserved: chi prefers it
// over {product}, so a product with that slug could never be reached.
func TestReservedCoversStaticUpdateSegments(t *testing.T) {
	for _, s := range []string{"manifest", "releases", "download", "updater", "all"} {
		if !Reserved(s) {
			t.Errorf("%q must be reserved", s)
		}
	}
	if Reserved(EMLy) {
		t.Error("emly must not be reserved")
	}
}

func TestS3Prefix(t *testing.T) {
	cases := []struct {
		name string
		p    models.Product
		base string
		want string
	}{
		{"emly keeps the legacy base", models.Product{Slug: "emly"}, "releases", "releases"},
		{"emly with empty base", models.Product{Slug: "emly"}, "", ""},
		{"other product gets its own folder", models.Product{Slug: "foo"}, "releases", "releases/foo"},
		{"other product with empty base", models.Product{Slug: "foo"}, "", "foo"},
		{"explicit prefix wins", models.Product{Slug: "foo", S3Prefix: strPtr("custom/foo")}, "releases", "custom/foo"},
		{"explicit prefix wins for emly too", models.Product{Slug: "emly", S3Prefix: strPtr("emly")}, "releases", "emly"},
		{"empty explicit prefix is ignored", models.Product{Slug: "foo", S3Prefix: strPtr("")}, "releases", "releases/foo"},
	}
	for _, c := range cases {
		if got := S3Prefix(c.p, c.base); got != c.want {
			t.Errorf("%s: S3Prefix = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNilRegistryKnowsOnlyEMLy(t *testing.T) {
	var r *Registry
	if p, ok := r.Get(EMLy); !ok || p.Slug != EMLy || !p.Enabled {
		t.Fatalf("nil registry Get(emly) = %+v, %v", p, ok)
	}
	if r.Has("foo") {
		t.Error("nil registry must not know foo")
	}
	if l := r.List(); len(l) != 1 || l[0].Slug != EMLy {
		t.Errorf("nil registry List = %+v", l)
	}
	if err := r.Reload(t.Context()); err != nil {
		t.Errorf("nil registry Reload = %v", err)
	}
}

func TestRegistrySetGetList(t *testing.T) {
	r := New(nil)
	r.Set([]models.Product{
		{Slug: "zeta", Name: "Zeta", Enabled: true},
		{Slug: "foo", Name: "Foo", Enabled: false},
	})
	if !r.Has("foo") || !r.Has("zeta") {
		t.Fatal("Set products must be known")
	}
	// emly is always there even when the table snapshot lacks it.
	if !r.Has(EMLy) {
		t.Fatal("emly must survive a snapshot without it")
	}
	l := r.List()
	if len(l) != 3 || l[0].Slug != "emly" || l[1].Slug != "foo" || l[2].Slug != "zeta" {
		t.Errorf("List = %+v, want emly, foo, zeta", l)
	}
}

func TestFromContextDefaultsToEMLy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := FromContext(req.Context()).Slug; got != EMLy {
		t.Errorf("FromContext without a product = %q, want emly", got)
	}
}

func resolveRouter(r *Registry, includeDisabled bool, got *string) http.Handler {
	router := chi.NewRouter()
	router.With(r.Resolve(includeDisabled)).Get("/{product}", func(w http.ResponseWriter, req *http.Request) {
		*got = FromContext(req.Context()).Slug
	})
	return router
}

func TestResolve(t *testing.T) {
	r := New(nil)
	r.Set([]models.Product{
		{Slug: "emly", Enabled: true},
		{Slug: "foo", Enabled: true},
		{Slug: "off", Enabled: false},
	})

	cases := []struct {
		path            string
		includeDisabled bool
		wantStatus      int
		wantSlug        string
	}{
		{"/foo", false, http.StatusOK, "foo"},
		{"/emly", false, http.StatusOK, "emly"},
		{"/nope", false, http.StatusNotFound, ""},
		{"/nope", true, http.StatusNotFound, ""},
		{"/off", false, http.StatusNotFound, ""},
		{"/off", true, http.StatusOK, "off"},
	}
	for _, c := range cases {
		var got string
		rec := httptest.NewRecorder()
		resolveRouter(r, c.includeDisabled, &got).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != c.wantStatus || got != c.wantSlug {
			t.Errorf("%s (includeDisabled=%v): status %d slug %q, want %d %q",
				c.path, c.includeDisabled, rec.Code, got, c.wantStatus, c.wantSlug)
		}
	}
}

// The legacy routes serve EMLy even if an operator disabled it: they did so
// unconditionally before products existed.
func TestFixedServesDisabledEMLy(t *testing.T) {
	r := New(nil)
	r.Set([]models.Product{{Slug: "emly", Enabled: false, S3Prefix: strPtr("x")}})

	var got models.Product
	h := r.Fixed(EMLy)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got = FromContext(req.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || got.Slug != EMLy || got.S3Prefix == nil || *got.S3Prefix != "x" {
		t.Errorf("Fixed(emly): status %d product %+v", rec.Code, got)
	}
}
