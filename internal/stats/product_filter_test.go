package stats

import (
	"reflect"
	"strings"
	"testing"

	"emly-api-go/internal/models"
	"emly-api-go/internal/productreg"
	"emly-api-go/internal/session"
)

func TestValidProduct(t *testing.T) {
	reg := productreg.New(nil)
	reg.Set([]models.Product{{Slug: "emly", Enabled: true}, {Slug: "foo", Enabled: false}})

	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "emly", true},
		{"emly", "emly", true},
		{"foo", "foo", true}, // disabled products keep their history
		{"updater", "updater", true},
		{"all", "all", true},
		{"bar", "bar", false},
	}
	for _, c := range cases {
		got, ok := validProduct(reg, c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("validProduct(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if _, ok := validProduct(nil, "foo"); ok {
		t.Error("a nil registry must only know emly")
	}
}

func TestProductFilter(t *testing.T) {
	open := session.Unrestricted()
	if clause, args := productFilter("all", open); clause != "" || args != nil {
		t.Errorf("all: %q %v", clause, args)
	}
	if clause, args := productFilter("foo", open); clause != " AND product = ?" || len(args) != 1 || args[0] != "foo" {
		t.Errorf("foo: %q %v", clause, args)
	}
	// A scoped "all" is "all of mine", plus the Agent's own traffic.
	scoped := session.Restricted("u1", "foo", "emly")
	clause, args := productFilter("all", scoped)
	if clause != " AND product IN (?, ?, ?)" || !reflect.DeepEqual(args, []interface{}{"emly", "foo", "updater"}) {
		t.Errorf("scoped all: %q %v", clause, args)
	}
}

func TestProductAllowed(t *testing.T) {
	s := session.Restricted("u1", "foo")
	for p, want := range map[string]bool{"foo": true, "emly": false, "all": true, "updater": true} {
		if got := productAllowed(s, p); got != want {
			t.Errorf("productAllowed(%q) = %v, want %v", p, got, want)
		}
	}
	if !productAllowed(session.Unrestricted(), "emly") {
		t.Error("unrestricted must allow everything")
	}
}

func TestClientScopeClause(t *testing.T) {
	if c, a := clientScopeClause(session.Unrestricted()); c != "" || a != nil {
		t.Errorf("unrestricted: %q %v", c, a)
	}
	if c, _ := clientScopeClause(session.Restricted("u1")); c != "0 = 1" {
		t.Errorf("no products must see no machine, got %q", c)
	}
	c, a := clientScopeClause(session.Restricted("u1", "foo", "emly"))
	if !strings.Contains(c, "cp.product IN (?, ?)") || !reflect.DeepEqual(a, []interface{}{"emly", "foo"}) {
		t.Errorf("scoped: %q %v", c, a)
	}
}
