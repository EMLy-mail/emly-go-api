package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
)

// ListUsers handles GET /v1/api/admin/users
func ListUsers(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var users []models.User
		if err := db.SelectContext(r.Context(), &users,
			"SELECT id, username, displayname, role, enabled, created_at FROM `user` ORDER BY created_at ASC",
		); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonOK(w, users)
	}
}

// CreateUser handles POST /v1/api/admin/users
func CreateUser(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username    string          `json:"username"`
			Displayname string          `json:"displayname"`
			Password    string          `json:"password"`
			Role        models.UserRole `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.Username == "" || body.Password == "" {
			jsonError(w, http.StatusBadRequest, "username and password are required")
			return
		}
		if body.Role != models.UserRoleAdmin && body.Role != models.UserRoleUser {
			jsonError(w, http.StatusBadRequest, "role must be 'admin' or 'user'")
			return
		}

		var count int
		if err := db.GetContext(r.Context(), &count,
			"SELECT COUNT(*) FROM `user` WHERE username = ?", body.Username,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if count > 0 {
			jsonError(w, http.StatusConflict, "username already exists")
			return
		}

		passwordHash, err := hashPassword(body.Password)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to hash password: "+err.Error())
			return
		}
		id, err := generateUUID()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to generate id: "+err.Error())
			return
		}

		if _, err := db.ExecContext(r.Context(),
			"INSERT INTO `user` (id, username, displayname, password_hash, role) VALUES (?, ?, ?, ?, ?)",
			id, body.Username, body.Displayname, passwordHash, body.Role,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		var user models.User
		if err := db.GetContext(r.Context(), &user,
			"SELECT id, username, displayname, role, enabled, created_at FROM `user` WHERE id = ?", id,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonCreated(w, user)
	}
}

// GetUserByID handles GET /v1/api/admin/users/{id}
func GetUserByID(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			jsonError(w, http.StatusBadRequest, "missing id parameter")
			return
		}

		var user models.User
		err := db.GetContext(r.Context(), &user,
			"SELECT id, username, displayname, role, enabled, created_at FROM `user` WHERE id = ? LIMIT 1", id,
		)
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "user not found")
			return
		}
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonOK(w, user)
	}
}

// UpdateUser handles PATCH /v1/api/admin/users/{id}
// Accepted fields: displayname, enabled
func UpdateUser(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			jsonError(w, http.StatusBadRequest, "missing id parameter")
			return
		}

		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		fields := []string{}
		params := []any{}

		if raw, ok := body["displayname"]; ok {
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				jsonError(w, http.StatusBadRequest, "invalid displayname value")
				return
			}
			fields = append(fields, "displayname = ?")
			params = append(params, v)
		}
		if raw, ok := body["enabled"]; ok {
			var v bool
			if err := json.Unmarshal(raw, &v); err != nil {
				jsonError(w, http.StatusBadRequest, "invalid enabled value")
				return
			}
			fields = append(fields, "enabled = ?")
			params = append(params, v)
		}

		if len(fields) == 0 {
			jsonError(w, http.StatusBadRequest, "no updatable fields provided")
			return
		}

		query := "UPDATE `user` SET "
		for i, f := range fields {
			if i > 0 {
				query += ", "
			}
			query += f
		}
		query += " WHERE id = ?"
		params = append(params, id)

		result, err := db.ExecContext(r.Context(), query, params...)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		rows, err := result.RowsAffected()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rows == 0 {
			jsonError(w, http.StatusNotFound, "user not found")
			return
		}
		jsonOK(w, map[string]bool{"updated": true})
	}
}

// ResetPassword handles POST /v1/api/admin/users/{id}/reset-password
func ResetPassword(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			jsonError(w, http.StatusBadRequest, "missing id parameter")
			return
		}

		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.Password == "" {
			jsonError(w, http.StatusBadRequest, "password is required")
			return
		}

		passwordHash, err := hashPassword(body.Password)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to hash password: "+err.Error())
			return
		}

		result, err := db.ExecContext(r.Context(),
			"UPDATE `user` SET password_hash = ? WHERE id = ?", passwordHash, id,
		)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		rows, err := result.RowsAffected()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rows == 0 {
			jsonError(w, http.StatusNotFound, "user not found")
			return
		}
		jsonOK(w, map[string]bool{"updated": true})
	}
}

// DeleteUser handles DELETE /v1/api/admin/users/{id}
func DeleteUser(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			jsonError(w, http.StatusBadRequest, "missing id parameter")
			return
		}

		result, err := db.ExecContext(r.Context(),
			"DELETE FROM `user` WHERE id = ?", id,
		)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		rows, err := result.RowsAffected()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rows == 0 {
			jsonError(w, http.StatusNotFound, "user not found")
			return
		}
		jsonOK(w, map[string]bool{"deleted": true})
	}
}
