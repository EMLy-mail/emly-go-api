// Package oidc verifies the ID token an OpenID Connect provider (Keycloak, in
// front of Active Directory) issued to the dashboard, and turns its group
// claim into a dashboard role.
//
// The browser flow itself lives in the dashboard; the API only ever sees the
// finished ID token, so it needs no client secret - just the issuer's public
// keys, fetched lazily and cached by go-oidc.
package oidc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"emly-api-go/internal/config"
	"emly-api-go/internal/models"
)

var (
	// ErrNoAccess means the token is valid but the user belongs to none of the
	// groups mapped to a role.
	ErrNoAccess = errors.New("user is not in any group allowed to sign in")
	// ErrNonceMismatch means the token was not minted for this login attempt.
	ErrNonceMismatch = errors.New("nonce mismatch")
)

// Identity is what a verified token says about the person.
type Identity struct {
	Subject     string
	Username    string
	Displayname string
	Role        models.UserRole
}

// Verifier checks ID tokens against one configured provider.
type Verifier struct {
	cfg config.OIDCConfig

	mu       sync.Mutex
	verifier *gooidc.IDTokenVerifier
}

func NewVerifier(cfg config.OIDCConfig) *Verifier {
	return &Verifier{cfg: cfg}
}

// Enabled reports whether SSO is configured at all.
func (v *Verifier) Enabled() bool { return v != nil && v.cfg.Enabled() }

// idTokenVerifier lazily runs provider discovery, so the API still boots when
// the provider is down and simply fails SSO logins until it is back.
func (v *Verifier) idTokenVerifier(ctx context.Context) (*gooidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.verifier != nil {
		return v.verifier, nil
	}
	provider, err := gooidc.NewProvider(ctx, v.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	v.verifier = provider.Verifier(&gooidc.Config{ClientID: v.cfg.ClientID})
	return v.verifier, nil
}

// Verify validates the token's signature, issuer, audience and expiry, checks
// its nonce and maps its groups to a role.
func (v *Verifier) Verify(ctx context.Context, rawIDToken, nonce string) (*Identity, error) {
	if !v.Enabled() {
		return nil, errors.New("sso is not configured")
	}
	iv, err := v.idTokenVerifier(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := iv.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("invalid id token: %w", err)
	}

	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("read claims: %w", err)
	}
	if n, _ := claims["nonce"].(string); nonce == "" || n != nonce {
		return nil, ErrNonceMismatch
	}

	groups := claimStrings(claims[v.cfg.GroupsClaim])
	role, ok := RoleForGroups(v.cfg, groups)
	if !ok {
		// Spell out what was compared: a refusal here is almost always a
		// missing group mapper in the provider or a name that does not match.
		slog.Warn("sso: no group grants access",
			"subject", tok.Subject,
			"groups_claim", v.cfg.GroupsClaim,
			"token_groups", groups,
			"owner_groups", v.cfg.OwnerGroups,
			"admin_groups", v.cfg.AdminGroups,
			"user_groups", v.cfg.UserGroups,
		)
		return nil, ErrNoAccess
	}

	username, _ := claims["preferred_username"].(string)
	if username == "" {
		username, _ = claims["email"].(string)
	}
	if username == "" {
		username = tok.Subject
	}
	name, _ := claims["name"].(string)
	return &Identity{Subject: tok.Subject, Username: username, Displayname: name, Role: role}, nil
}

// RoleForGroups returns the highest role granted by any of the user's groups.
// Keycloak's group mapper can emit paths ("/aryx-admins"), so a leading slash
// is ignored, and matching is case-insensitive as AD group names are.
func RoleForGroups(cfg config.OIDCConfig, groups []string) (models.UserRole, bool) {
	has := func(wanted []string) bool {
		for _, w := range wanted {
			for _, g := range groups {
				if strings.EqualFold(strings.TrimPrefix(g, "/"), strings.TrimPrefix(w, "/")) {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has(cfg.OwnerGroups):
		return models.UserRoleOwner, true
	case has(cfg.AdminGroups):
		return models.UserRoleAdmin, true
	case has(cfg.UserGroups):
		return models.UserRoleUser, true
	}
	return "", false
}

// claimStrings reads a claim that may be a JSON array or a single string.
func claimStrings(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// backchannelLogoutEvent is the marker every OIDC back-channel logout token
// carries in its `events` claim (OpenID Connect Back-Channel Logout 1.0 §2.4).
const backchannelLogoutEvent = "http://schemas.openid.net/event/backchannel-logout"

var ErrNotLogoutToken = errors.New("not a back-channel logout token")

// VerifyLogoutToken validates a logout token sent by the provider when a user's
// session there ends, and returns the subject to sign out. Signature, issuer,
// audience and expiry are checked like an ID token's; on top of that the spec
// requires the logout event, forbids a nonce (so an ID token cannot be replayed
// as one) and needs a subject to know whose sessions to drop.
func (v *Verifier) VerifyLogoutToken(ctx context.Context, rawToken string) (string, error) {
	if !v.Enabled() {
		return "", errors.New("sso is not configured")
	}
	iv, err := v.idTokenVerifier(ctx)
	if err != nil {
		return "", err
	}
	tok, err := iv.Verify(ctx, rawToken)
	if err != nil {
		return "", fmt.Errorf("invalid logout token: %w", err)
	}
	var claims struct {
		Events map[string]any `json:"events"`
		Nonce  *string        `json:"nonce"`
	}
	if err := tok.Claims(&claims); err != nil {
		return "", fmt.Errorf("read claims: %w", err)
	}
	if _, ok := claims.Events[backchannelLogoutEvent]; !ok || claims.Nonce != nil {
		return "", ErrNotLogoutToken
	}
	if tok.Subject == "" {
		return "", errors.New("logout token has no subject")
	}
	return tok.Subject, nil
}
