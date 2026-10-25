package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/config"
	"emly-api-go/internal/models"
	"emly-api-go/internal/oidc"
	"emly-api-go/internal/response"
	"emly-api-go/internal/session"
)

// unusableHash is stored as password_hash for SSO accounts. It is not a valid
// PHC string, so verifyPassword always rejects it and a local login can never
// succeed for them.
const unusableHash = "!"

// LoginOIDC handles POST /v2/admin/auth/oidc.
//
// The dashboard completes the authorization-code flow with the identity
// provider and posts the resulting ID token here (with the nonce it started
// the flow with). The API verifies it, creates or updates the matching user
// from the token's groups, and opens a normal session - so everything after
// login is identical to a password login.
func LoginOIDC(db *sqlx.DB, verifier *oidc.Verifier, cfg config.OIDCConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !verifier.Enabled() {
			response.Error(w, http.StatusNotFound, "sso is not enabled")
			return
		}
		var body struct {
			IDToken string `json:"id_token"`
			Nonce   string `json:"nonce"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.IDToken == "" || body.Nonce == "" {
			response.Error(w, http.StatusBadRequest, "id_token and nonce are required")
			return
		}

		id, err := verifier.Verify(r.Context(), body.IDToken, body.Nonce)
		switch {
		case errors.Is(err, oidc.ErrNoAccess):
			response.Error(w, http.StatusForbidden, "your account is not allowed to access this console")
			return
		case err != nil:
			slog.WarnContext(r.Context(), "oidc login rejected", "err", err)
			response.Error(w, http.StatusUnauthorized, "invalid sso token")
			return
		}

		user, status, err := upsertOIDCUser(r, db, id)
		if err != nil {
			response.Error(w, status, err.Error())
			return
		}

		sessionID, err := generateSessionID()
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to generate session: "+err.Error())
			return
		}
		expiresAt := time.Now().UTC().Add(cfg.SessionDuration)
		if _, err := db.ExecContext(r.Context(),
			"INSERT INTO session (id, user_id, expires_at) VALUES (?, ?, ?)",
			sessionID, user.ID, expiresAt,
		); err != nil {
			response.Error(w, http.StatusInternalServerError, err.Error())
			return
		}

		slog.InfoContext(r.Context(), "user logged in via sso", "username", user.Username, "role", user.Role, "session_prefix", sessionID[:8])
		response.OK(w, map[string]any{"session_id": sessionID, "user": user})
	}
}

// upsertOIDCUser finds the account for this provider identity, creating it on
// first login. Role and display name are refreshed on every login so a change
// of AD group takes effect at the next sign-in. On failure it returns the
// HTTP status to answer with.
func upsertOIDCUser(r *http.Request, db *sqlx.DB, id *oidc.Identity) (authUser, int, error) {
	ctx := r.Context()

	var existing struct {
		ID       string `db:"id"`
		Username string `db:"username"`
		Enabled  bool   `db:"enabled"`
	}
	err := db.GetContext(ctx, &existing,
		"SELECT id, username, enabled FROM `user` WHERE external_id = ? AND auth_provider = ? LIMIT 1",
		id.Subject, models.AuthProviderOIDC,
	)
	switch {
	case err == nil:
		if !existing.Enabled {
			return authUser{}, http.StatusForbidden, errors.New("account disabled")
		}
		if _, err := db.ExecContext(ctx,
			"UPDATE `user` SET role = ?, displayname = ? WHERE id = ?",
			id.Role, id.Displayname, existing.ID,
		); err != nil {
			return authUser{}, http.StatusInternalServerError, err
		}
		return authUser{
			ID: existing.ID, Username: existing.Username, Displayname: id.Displayname,
			Role: id.Role, Enabled: true, AuthProvider: models.AuthProviderOIDC,
		}, 0, nil
	case !errors.Is(err, sql.ErrNoRows):
		return authUser{}, http.StatusInternalServerError, err
	}

	// A local account an admin migrated (see MigrateUserToSSO) is waiting for
	// its first SSO login: same username, no subject yet. That login claims it
	// for good, keeping its id and everything attached to it.
	var pending struct {
		ID      string `db:"id"`
		Enabled bool   `db:"enabled"`
	}
	err = db.GetContext(ctx, &pending,
		"SELECT id, enabled FROM `user` WHERE username = ? AND auth_provider = ? AND external_id IS NULL LIMIT 1",
		id.Username, models.AuthProviderOIDC,
	)
	switch {
	case err == nil:
		if !pending.Enabled {
			return authUser{}, http.StatusForbidden, errors.New("account disabled")
		}
		res, err := db.ExecContext(ctx,
			"UPDATE `user` SET external_id = ?, role = ?, displayname = ? WHERE id = ? AND external_id IS NULL",
			id.Subject, id.Role, id.Displayname, pending.ID,
		)
		if err != nil {
			return authUser{}, http.StatusInternalServerError, err
		}
		// Another login claimed it between the read and the write.
		if n, _ := res.RowsAffected(); n == 0 {
			return authUser{}, http.StatusConflict, errors.New("this account was just linked to another sso identity")
		}
		slog.InfoContext(ctx, "sso: migrated account linked", "username", id.Username, "user_id", pending.ID)
		return authUser{
			ID: pending.ID, Username: id.Username, Displayname: id.Displayname,
			Role: id.Role, Enabled: true, AuthProvider: models.AuthProviderOIDC,
		}, 0, nil
	case !errors.Is(err, sql.ErrNoRows):
		return authUser{}, http.StatusInternalServerError, err
	}

	// Otherwise SSO users are always new accounts: an SSO identity never takes
	// over a local account that happens to share its username.
	var taken int
	if err := db.GetContext(ctx, &taken, "SELECT COUNT(*) FROM `user` WHERE username = ?", id.Username); err != nil {
		return authUser{}, http.StatusInternalServerError, err
	}
	if taken > 0 {
		return authUser{}, http.StatusConflict, errors.New("a local account with this username already exists")
	}

	newID, err := generateUUID()
	if err != nil {
		return authUser{}, http.StatusInternalServerError, err
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `user` (id, username, displayname, password_hash, role, auth_provider, external_id) VALUES (?, ?, ?, ?, ?, ?, ?)",
		newID, id.Username, id.Displayname, unusableHash, id.Role, models.AuthProviderOIDC, id.Subject,
	); err != nil {
		return authUser{}, http.StatusInternalServerError, err
	}
	return authUser{
		ID: newID, Username: id.Username, Displayname: id.Displayname,
		Role: id.Role, Enabled: true, AuthProvider: models.AuthProviderOIDC,
	}, 0, nil
}

// MigrateUserToSSO handles POST /v2/admin/users/{id}/migrate-sso.
//
// Turns a local account into an SSO one waiting to be claimed: the password
// stops working at once (and every open session is dropped), and the next SSO
// login with the same username takes the account over, keeping its id,
// products and history (see upsertOIDCUser). An admin choosing this per
// account is what makes the username match safe; it never happens on its own.
//
// Refused while SSO is off, since it would leave the user no way in.
func MigrateUserToSSO(db *sqlx.DB, verifier *oidc.Verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !verifier.Enabled() {
			response.Error(w, http.StatusConflict, "sso is not enabled")
			return
		}
		if token := r.Header.Get(session.Header); token != "" {
			role, err := sessionRole(r, db, token)
			if err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to resolve session")
				return
			}
			if role != models.UserRoleOwner && role != models.UserRoleAdmin {
				response.Error(w, http.StatusForbidden, "only admins and owners can migrate users")
				return
			}
		}

		id := chi.URLParam(r, "id")
		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer tx.Rollback()

		res, err := tx.ExecContext(r.Context(),
			"UPDATE `user` SET auth_provider = ?, password_hash = ?, external_id = NULL WHERE id = ? AND auth_provider = ?",
			models.AuthProviderOIDC, unusableHash, id, models.AuthProviderLocal,
		)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if ok, err := userExists(r, db, id); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to fetch user")
			} else if !ok {
				response.Error(w, http.StatusNotFound, "user not found")
			} else {
				response.Error(w, http.StatusConflict, "user already signs in with sso")
			}
			return
		}
		// Sessions opened with the password must not outlive it.
		if _, err := tx.ExecContext(r.Context(), "DELETE FROM session WHERE user_id = ?", id); err != nil {
			response.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			response.Error(w, http.StatusInternalServerError, err.Error())
			return
		}

		slog.InfoContext(r.Context(), "user migrated to sso", "user_id", id)
		response.OK(w, map[string]bool{"migrated": true})
	}
}

// BackchannelLogoutOIDC handles POST /v2/admin/auth/oidc/backchannel-logout.
//
// When a user signs out at the identity provider, it tells the dashboard (which
// forwards the signed logout token here). Every session that user has on this
// console is dropped, so signing out of Keycloak signs them out of Aryx too.
// Unknown subjects are a success: the user may simply never have used the
// console, and the provider must not see an error for that.
func BackchannelLogoutOIDC(db *sqlx.DB, verifier *oidc.Verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !verifier.Enabled() {
			response.Error(w, http.StatusNotFound, "sso is not enabled")
			return
		}
		var body struct {
			LogoutToken string `json:"logout_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.LogoutToken == "" {
			response.Error(w, http.StatusBadRequest, "logout_token is required")
			return
		}

		sub, err := verifier.VerifyLogoutToken(r.Context(), body.LogoutToken)
		if err != nil {
			slog.WarnContext(r.Context(), "oidc back-channel logout rejected", "err", err)
			response.Error(w, http.StatusBadRequest, "invalid logout token")
			return
		}

		res, err := db.ExecContext(r.Context(),
			`DELETE s FROM session s JOIN `+"`user`"+` u ON u.id = s.user_id
			 WHERE u.external_id = ? AND u.auth_provider = ?`,
			sub, models.AuthProviderOIDC,
		)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		n, _ := res.RowsAffected()
		slog.InfoContext(r.Context(), "sso back-channel logout", "sessions_removed", n)
		response.OK(w, map[string]int64{"sessions_removed": n})
	}
}
