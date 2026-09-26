package admin

import (
	"testing"

	"emly-api-go/internal/models"
	"emly-api-go/internal/session"
)

func TestCanManageUser(t *testing.T) {
	owner := &session.Actor{ID: "o", Role: models.UserRoleOwner}
	admin := &session.Actor{ID: "a", Role: models.UserRoleAdmin}
	user := &session.Actor{ID: "u", Role: models.UserRoleUser}

	cases := []struct {
		name       string
		actor      *session.Actor
		targetID   string
		targetRole models.UserRole
		want       bool
	}{
		{"owner manages owner", owner, "o2", models.UserRoleOwner, true},
		{"owner manages admin", owner, "a2", models.UserRoleAdmin, true},
		{"admin manages user", admin, "u2", models.UserRoleUser, true},
		{"admin not another admin", admin, "a2", models.UserRoleAdmin, false},
		{"admin not an owner", admin, "o", models.UserRoleOwner, false},
		{"admin manages self", admin, "a", models.UserRoleAdmin, true},
		{"user manages self", user, "u", models.UserRoleUser, true},
		{"user not others", user, "u2", models.UserRoleUser, false},
	}
	for _, c := range cases {
		if got := canManageUser(c.actor, c.targetID, c.targetRole); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
