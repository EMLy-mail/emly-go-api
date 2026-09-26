package admin

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
	"emly-api-go/internal/response"
	"emly-api-go/internal/session"
)

// canManageUser is the rule for changing another account (disable/enable,
// reset password, delete): an owner manages anyone, an admin manages ordinary
// users only - never another admin or an owner - and everyone may act on their
// own account. Mirrors canManageUser in the dashboard's lib/roles.ts.
func canManageUser(actor *session.Actor, targetID string, targetRole models.UserRole) bool {
	if actor.ID == targetID || actor.Role == models.UserRoleOwner {
		return true
	}
	return actor.Role == models.UserRoleAdmin && targetRole == models.UserRoleUser
}

// ManageGuard enforces canManageUser on the routes that act on /{id}. It only
// binds requests made on behalf of a signed-in user (session token present);
// a bare admin-key call is automation and is left alone, as elsewhere.
func ManageGuard(db *sqlx.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, err := session.Resolve(r, db)
			switch {
			case errors.Is(err, session.ErrInvalidSession):
				response.Error(w, http.StatusUnauthorized, "invalid session")
				return
			case err != nil:
				response.Error(w, http.StatusInternalServerError, "internal server error")
				return
			case actor == nil:
				next.ServeHTTP(w, r)
				return
			}

			targetID := chi.URLParam(r, "id")
			var targetRole models.UserRole
			err = db.GetContext(r.Context(), &targetRole, "SELECT role FROM `user` WHERE id = ? LIMIT 1", targetID)
			if errors.Is(err, sql.ErrNoRows) {
				next.ServeHTTP(w, r) // the handler answers 404
				return
			}
			if err != nil {
				response.Error(w, http.StatusInternalServerError, "internal server error")
				return
			}
			if !canManageUser(actor, targetID, targetRole) {
				response.Error(w, http.StatusForbidden, "you cannot manage this user")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
