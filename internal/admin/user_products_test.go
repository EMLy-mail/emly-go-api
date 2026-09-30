package admin

import (
	"testing"

	"emly-api-go/internal/models"
)

// Owner sits above admin in the dashboard's hierarchy, so it must be able to
// do anything an admin can - including assigning products.
func TestCanAssignProducts(t *testing.T) {
	cases := map[models.UserRole]bool{
		models.UserRoleOwner: true,
		models.UserRoleAdmin: true,
		models.UserRoleUser:  false,
		"":                   false, // unknown/expired/disabled session
		"superuser":          false,
	}
	for role, want := range cases {
		if got := canAssignProducts(role); got != want {
			t.Errorf("canAssignProducts(%q) = %v, want %v", role, got, want)
		}
	}
}
