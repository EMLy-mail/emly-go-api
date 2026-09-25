package oidc

import (
	"testing"

	"emly-api-go/internal/config"
	"emly-api-go/internal/models"
)

func TestRoleForGroups(t *testing.T) {
	cfg := config.OIDCConfig{
		OwnerGroups: []string{"aryx-owners"},
		AdminGroups: []string{"aryx-admins"},
		UserGroups:  []string{"aryx-users", "/Domain Users/EMLy"},
	}
	cases := []struct {
		name   string
		groups []string
		want   models.UserRole
		ok     bool
	}{
		{"none", []string{"other"}, "", false},
		{"empty", nil, "", false},
		{"user", []string{"aryx-users"}, models.UserRoleUser, true},
		{"path form", []string{"/aryx-admins"}, models.UserRoleAdmin, true},
		{"case insensitive", []string{"ARYX-USERS"}, models.UserRoleUser, true},
		{"highest wins", []string{"aryx-users", "aryx-owners", "aryx-admins"}, models.UserRoleOwner, true},
		{"configured path", []string{"Domain Users/EMLy"}, models.UserRoleUser, true},
	}
	for _, c := range cases {
		got, ok := RoleForGroups(cfg, c.groups)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got (%q,%v), want (%q,%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestClaimStrings(t *testing.T) {
	if got := claimStrings([]any{"a", 1, "b"}); len(got) != 2 || got[1] != "b" {
		t.Errorf("array claim: %v", got)
	}
	if got := claimStrings("solo"); len(got) != 1 || got[0] != "solo" {
		t.Errorf("string claim: %v", got)
	}
	if claimStrings(nil) != nil {
		t.Error("nil claim should yield nil")
	}
}
