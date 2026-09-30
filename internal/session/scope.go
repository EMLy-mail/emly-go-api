package session

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
	"emly-api-go/internal/response"
)

// Scope is which products a request may see (the user_products table).
//
// Unlike Username, this is enforcement, not attribution, so the rules are
// strict where Username is lenient:
//   - no X-Session-Token at all: unrestricted. That is a caller holding the
//     admin key with no user behind it - a script, an integration - and the
//     admin key can already do anything;
//   - a token that resolves to an enabled owner: unrestricted. Owner is the
//     top of the hierarchy (owner > admin > user) and sees every product
//     without assignments (unrestrictedRole);
//   - a token that resolves to any other enabled user: exactly that user's
//     assigned products, admins included - below owner the role does not
//     widen it;
//   - a token that is unknown, expired or belongs to a disabled user:
//     restricted to nothing. Falling back to "unrestricted" would let any
//     dashboard request carrying a stale token see everything.
type Scope struct {
	restricted bool
	products   map[string]bool
	// UserID is the user the token resolved to, "" when there is none.
	UserID string
}

// Unrestricted is the scope of a request without a session token.
func Unrestricted() Scope { return Scope{} }

// Restricted builds a scope limited to products. Exported for tests and for
// callers that already know the assignment.
func Restricted(userID string, products ...string) Scope {
	m := make(map[string]bool, len(products))
	for _, p := range products {
		m[p] = true
	}
	return Scope{restricted: true, products: m, UserID: userID}
}

// IsRestricted reports whether the scope limits anything.
func (s Scope) IsRestricted() bool { return s.restricted }

// Allows reports whether product is visible in this scope.
func (s Scope) Allows(product string) bool { return !s.restricted || s.products[product] }

// Products returns the assigned products, sorted. Meaningless (nil) for an
// unrestricted scope - check IsRestricted first.
func (s Scope) Products() []string {
	if !s.restricted {
		return nil
	}
	out := make([]string, 0, len(s.products))
	for p := range s.products {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Key identifies the scope for a cache key: two requests with the same Key
// see the same data.
func (s Scope) Key() string {
	if !s.restricted {
		return "*"
	}
	return strings.Join(s.Products(), ",")
}

// ScopeFromToken resolves token to a Scope. It returns an error only for a
// database failure; every other way a token can fail is a restricted-to-
// nothing scope (see Scope). db may be nil (tests): a token then resolves to
// nothing.
func ScopeFromToken(ctx context.Context, db *sqlx.DB, token string) (Scope, error) {
	if token == "" {
		return Unrestricted(), nil
	}
	if db == nil {
		return Restricted(""), nil
	}

	var row struct {
		UserID    string          `db:"user_id"`
		Role      models.UserRole `db:"role"`
		Enabled   bool            `db:"enabled"`
		ExpiresAt time.Time       `db:"expires_at"`
	}
	err := db.GetContext(ctx, &row,
		"SELECT s.user_id, u.role, u.enabled, s.expires_at FROM session s JOIN `user` u ON u.id = s.user_id WHERE s.id = ? LIMIT 1",
		token)
	if errors.Is(err, sql.ErrNoRows) {
		return Restricted(""), nil
	}
	if err != nil {
		return Scope{}, err
	}
	if !row.Enabled || time.Now().UTC().After(row.ExpiresAt) {
		return Restricted(""), nil
	}
	if unrestrictedRole(row.Role) {
		// UserID is kept: a product an owner creates is still assigned to
		// them, so it survives a later demotion.
		return Scope{UserID: row.UserID}, nil
	}

	products, err := UserProducts(ctx, db, row.UserID)
	if err != nil {
		return Scope{}, err
	}
	return Restricted(row.UserID, products...), nil
}

// unrestrictedRole reports whether a role sees every product regardless of
// user_products. Only owner does.
func unrestrictedRole(role models.UserRole) bool { return role == models.UserRoleOwner }

// VisibleProducts is what a user sees in the dashboard's product switcher:
// every product for an owner, the assigned ones for anyone else - the same
// answer ScopeFromToken enforces.
func VisibleProducts(ctx context.Context, db *sqlx.DB, userID string, role models.UserRole) ([]string, error) {
	if !unrestrictedRole(role) {
		return UserProducts(ctx, db, userID)
	}
	products := []string{}
	err := db.SelectContext(ctx, &products, `SELECT slug FROM products ORDER BY slug`)
	return products, err
}

// UserProducts returns the products assigned to userID, sorted.
func UserProducts(ctx context.Context, db *sqlx.DB, userID string) ([]string, error) {
	products := []string{}
	err := db.SelectContext(ctx, &products,
		`SELECT product FROM user_products WHERE user_id = ? ORDER BY product`, userID)
	return products, err
}

type scopeKey struct{}

// WithScope returns ctx carrying s.
func WithScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFrom returns the scope LoadScope put on ctx. A request that never went
// through LoadScope is unrestricted - which is why every route that must be
// scoped mounts LoadScope rather than trusting this default.
func ScopeFrom(ctx context.Context) Scope {
	if s, ok := ctx.Value(scopeKey{}).(Scope); ok {
		return s
	}
	return Unrestricted()
}

// LoadScope resolves the request's session token once and puts the Scope on
// its context. Mount it after the admin-key check: it only narrows what an
// authorized caller sees, it does not authorize anyone.
func LoadScope(db *sqlx.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s, err := ScopeFromToken(r.Context(), db, r.Header.Get(Header))
			if err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to resolve session scope")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithScope(r.Context(), s)))
		})
	}
}
