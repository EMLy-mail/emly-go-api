package session

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"emly-api-go/internal/models"
)

func TestScopeWithoutTokenIsUnrestricted(t *testing.T) {
	s, err := ScopeFromToken(t.Context(), nil, "")
	if err != nil || s.IsRestricted() || !s.Allows("anything") || s.Key() != "*" {
		t.Fatalf("no token: %+v %v", s, err)
	}
}

// A token that cannot be resolved must never widen to unrestricted.
func TestUnresolvableTokenSeesNothing(t *testing.T) {
	s, err := ScopeFromToken(t.Context(), nil, "some-token")
	if err != nil || !s.IsRestricted() || s.Allows("emly") {
		t.Fatalf("unresolvable token: %+v %v", s, err)
	}
}

func TestRestrictedScope(t *testing.T) {
	s := Restricted("u1", "foo", "emly")
	if !s.Allows("emly") || !s.Allows("foo") || s.Allows("bar") {
		t.Errorf("Allows wrong for %+v", s)
	}
	if got := s.Products(); !reflect.DeepEqual(got, []string{"emly", "foo"}) {
		t.Errorf("Products = %v", got)
	}
	if s.Key() != "emly,foo" {
		t.Errorf("Key = %q", s.Key())
	}
	if Restricted("u1").Key() == Unrestricted().Key() {
		t.Error("an empty scope must not share the unrestricted cache key")
	}
}

func TestLoadScopePutsScopeOnContext(t *testing.T) {
	var got Scope
	h := LoadScope(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = ScopeFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.IsRestricted() {
		t.Error("no token must load an unrestricted scope")
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(Header, "tok")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !got.IsRestricted() {
		t.Error("a token must load a restricted scope")
	}
}

// Owner is the top of the hierarchy and sees every product without
// assignments; every role below it is limited to user_products.
func TestUnrestrictedRole(t *testing.T) {
	cases := map[models.UserRole]bool{
		models.UserRoleOwner: true,
		models.UserRoleAdmin: false,
		models.UserRoleUser:  false,
		"":                   false,
	}
	for role, want := range cases {
		if got := unrestrictedRole(role); got != want {
			t.Errorf("unrestrictedRole(%q) = %v, want %v", role, got, want)
		}
	}
}
