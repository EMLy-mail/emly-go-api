// Package session resolves the dashboard user behind a request's session
// token.
//
// It is its own package because three features need it and none of them owns
// it: internal/admin issues and validates these tokens, while internal/bans and
// internal/configapi only want to know who to attribute a write to. Leaving the
// lookup inside admin would have every feature that records an author import
// the whole login/password surface to read one header.
//
// The lookup is deliberately best-effort - see Username.
package session

import (
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
)

// Header is the header name the dashboard passes its session ID in.
const Header = "X-Session-Token"

// Username resolves the dashboard user attached to the request's session
// token, if any. It is best-effort attribution (remote-config API design doc
// §7.2): a request authenticated by admin key alone - no session token, or an
// invalid or expired one - is attributed to nobody rather than rejected, so
// automation keeps working and only the author field goes empty.
func Username(r *http.Request, db *sqlx.DB) *string {
	token := r.Header.Get(Header)
	if token == "" {
		return nil
	}
	var row struct {
		Username  string    `db:"username"`
		ExpiresAt time.Time `db:"expires_at"`
	}
	err := db.GetContext(r.Context(), &row,
		`SELECT u.username, s.expires_at FROM session s JOIN user u ON u.id = s.user_id WHERE s.id = ? LIMIT 1`,
		token,
	)
	if err != nil || time.Now().UTC().After(row.ExpiresAt) {
		return nil
	}
	return &row.Username
}
