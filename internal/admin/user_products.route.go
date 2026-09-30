package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
	"emly-api-go/internal/response"
	"emly-api-go/internal/session"
)

// GetUserProducts handles GET /v2/api/admin/users/{id}/products: the products
// assigned to one user (user_products), which is what session.Scope limits
// their dashboard requests to.
func GetUserProducts(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if ok, err := userExists(r, db, id); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch user")
			return
		} else if !ok {
			response.Error(w, http.StatusNotFound, "user not found")
			return
		}

		products, err := session.UserProducts(r.Context(), db, id)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch user products")
			return
		}
		response.OK(w, map[string]any{"user_id": id, "products": products})
	}
}

// PutUserProducts handles PUT /v2/api/admin/users/{id}/products with
// {"products": ["emly", "foo"]}: it replaces the user's whole assignment.
//
// Only a caller with no session (the admin key alone) or a session whose role
// passes canAssignProducts may change assignments. Visibility is scoped for
// admins and owners too, but letting a plain user widen their own scope
// through this route would make the scope decorative.
func PutUserProducts(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token := r.Header.Get(session.Header); token != "" {
			role, err := sessionRole(r, db, token)
			if err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to resolve session")
				return
			}
			if !canAssignProducts(role) {
				response.Error(w, http.StatusForbidden, "only admins and owners can assign products")
				return
			}
		}

		id := chi.URLParam(r, "id")
		var body struct {
			Products []string `json:"products"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.Products == nil {
			response.Error(w, http.StatusBadRequest, "products is required (use [] to remove every product)")
			return
		}

		want := dedupe(body.Products)
		if len(want) > 0 {
			query, args, err := sqlx.In(`SELECT slug FROM products WHERE slug IN (?)`, want)
			if err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to validate products")
				return
			}
			var known []string
			if err := db.SelectContext(r.Context(), &known, db.Rebind(query), args...); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to validate products")
				return
			}
			if missing := difference(want, known); len(missing) > 0 {
				response.Error(w, http.StatusBadRequest, "unknown products: "+joinComma(missing))
				return
			}
		}

		if ok, err := userExists(r, db, id); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch user")
			return
		} else if !ok {
			response.Error(w, http.StatusNotFound, "user not found")
			return
		}

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback()

		if _, err := tx.ExecContext(r.Context(), `DELETE FROM user_products WHERE user_id = ?`, id); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to clear user products")
			return
		}
		for _, p := range want {
			if _, err := tx.ExecContext(r.Context(),
				`INSERT INTO user_products (user_id, product) VALUES (?, ?)`, id, p); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to assign product")
				return
			}
		}
		if err := tx.Commit(); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		response.OK(w, map[string]any{"user_id": id, "products": want})
	}
}

// canAssignProducts is who may change a user's product assignment: owner and
// admin (the dashboard's hierarchy is owner > admin > user, and an owner can do
// anything an admin can). Mirrors canAssignProducts in the dashboard's
// lib/roles.ts; the "" of an unresolvable session falls through to false.
func canAssignProducts(role models.UserRole) bool {
	return role == models.UserRoleOwner || role == models.UserRoleAdmin
}

func userExists(r *http.Request, db *sqlx.DB, id string) (bool, error) {
	var one int
	err := db.GetContext(r.Context(), &one, "SELECT 1 FROM `user` WHERE id = ? LIMIT 1", id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// sessionRole is the role behind a live session token, "" when the token is
// unknown, expired or belongs to a disabled user.
func sessionRole(r *http.Request, db *sqlx.DB, token string) (models.UserRole, error) {
	var role models.UserRole
	err := db.GetContext(r.Context(), &role,
		"SELECT u.role FROM session s JOIN `user` u ON u.id = s.user_id WHERE s.id = ? AND s.expires_at > UTC_TIMESTAMP() AND u.enabled = 1 LIMIT 1",
		token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func difference(want, have []string) []string {
	h := map[string]bool{}
	for _, s := range have {
		h[s] = true
	}
	var out []string
	for _, s := range want {
		if !h[s] {
			out = append(out, s)
		}
	}
	return out
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}
