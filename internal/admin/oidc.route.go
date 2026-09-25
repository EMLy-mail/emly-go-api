package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/config"
	"emly-api-go/internal/models"
	"emly-api-go/internal/oidc"
	"emly-api-go/internal/response"
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

	// New SSO users are always new accounts: an SSO identity never takes over
	// a local account that happens to share its username.
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
