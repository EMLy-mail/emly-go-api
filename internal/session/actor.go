package session

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
	"emly-api-go/internal/response"
)

// Actor is the dashboard user a request acts as.
type Actor struct {
	ID       string
	Username string
	Role     models.UserRole
}

// ErrInvalidSession means a session token was sent but does not identify an
// active user: unknown, expired, or belonging to a disabled account.
var ErrInvalidSession = errors.New("invalid session")

// Resolve identifies the acting user from the session token header.
//
// Unlike Username, which is best-effort attribution, this is for enforcing
// permissions, so the two failure modes are kept apart: no token at all gives
// (nil, nil) - a call authenticated by admin key alone, i.e. automation, which
// the role rules deliberately do not constrain - while a token that is present
// but bad gives ErrInvalidSession and must be refused, never treated as "no
// token", or sending garbage would sidestep the rules.
func Resolve(r *http.Request, db *sqlx.DB) (*Actor, error) {
	token := r.Header.Get(Header)
	if token == "" {
		return nil, nil
	}
	var row struct {
		ID        string          `db:"id"`
		Username  string          `db:"username"`
		Role      models.UserRole `db:"role"`
		Enabled   bool            `db:"enabled"`
		ExpiresAt time.Time       `db:"expires_at"`
	}
	err := db.GetContext(r.Context(), &row,
		"SELECT u.id, u.username, u.role, u.enabled, s.expires_at FROM session s JOIN `user` u ON u.id = s.user_id WHERE s.id = ? LIMIT 1",
		token,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidSession
	}
	if err != nil {
		return nil, err
	}
	if !row.Enabled || time.Now().UTC().After(row.ExpiresAt) {
		return nil, ErrInvalidSession
	}
	return &Actor{ID: row.ID, Username: row.Username, Role: row.Role}, nil
}

// RequireOwner refuses requests made on behalf of a signed-in user who is not
// an owner. Requests without a session token pass (admin-key automation).
// Used for features that are owner-only, such as the beta remote control.
func RequireOwner(db *sqlx.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, err := Resolve(r, db)
			switch {
			case errors.Is(err, ErrInvalidSession):
				response.Error(w, http.StatusUnauthorized, "invalid session")
				return
			case err != nil:
				response.Error(w, http.StatusInternalServerError, "internal server error")
				return
			case actor != nil && actor.Role != models.UserRoleOwner:
				response.Error(w, http.StatusForbidden, "owner role required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
