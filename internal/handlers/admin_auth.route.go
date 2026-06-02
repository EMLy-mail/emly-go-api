package handlers

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/argon2"

	"emly-api-go/internal/models"
)

// argon2id params — mirror @node-rs/argon2 defaults used in the TypeScript service.
const (
	argonMemory      = 19456
	argonTime        = 2
	argonKeyLen      = 32
	argonParallelism = 1
	argonSaltLen     = 16

	sessionExpiryDays = 30
)

// hashPassword produces a PHC-formatted argon2id string compatible with
// the @node-rs/argon2 library used in the TypeScript service.
func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonParallelism, argonKeyLen)
	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Hash := base64.RawStdEncoding.EncodeToString(hash)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonParallelism, b64Salt, b64Hash), nil
}

// verifyPassword checks a password against a PHC-formatted argon2id hash.
func verifyPassword(phc, password string) (bool, error) {
	parts := strings.Split(phc, "$")
	// Expected: ["", "argon2id", "v=19", "m=...,t=...,p=...", "<salt>", "<hash>"]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, fmt.Errorf("invalid hash format")
	}
	var memory, timeCost uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &parallelism); err != nil {
		return false, fmt.Errorf("invalid params: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("invalid salt: %w", err)
	}
	hashBytes, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("invalid hash: %w", err)
	}
	computed := argon2.IDKey([]byte(password), salt, timeCost, memory, parallelism, uint32(len(hashBytes)))
	return subtle.ConstantTimeCompare(computed, hashBytes) == 1, nil
}

// generateUUID generates a random UUID v4.
func generateUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// generateSessionID returns a 64-char hex string (32 random bytes),
// matching the TypeScript generateSessionId() implementation.
func generateSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// authUser is the public representation returned to callers after login/validate.
type authUser struct {
	ID          string          `json:"id"`
	Username    string          `json:"username"`
	Displayname string          `json:"displayname"`
	Role        models.UserRole `json:"role"`
	Enabled     bool            `json:"enabled"`
}

// sessionHeader is the header name used to pass the session ID.
const sessionHeader = "X-Session-Token"

// LoginUser handles POST /v1/api/admin/auth/login
func LoginUser(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.Username == "" || body.Password == "" {
			jsonError(w, http.StatusBadRequest, "username and password are required")
			return
		}

		var row struct {
			models.User
			PasswordHash string `db:"password_hash"`
		}
		err := db.GetContext(r.Context(), &row,
			"SELECT id, username, displayname, password_hash, role, enabled FROM `user` WHERE username = ? LIMIT 1",
			body.Username,
		)
		if err != nil {
			// Return 401 whether the user doesn't exist or query failed to avoid enumeration
			jsonError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}

		valid, err := verifyPassword(row.PasswordHash, body.Password)
		if err != nil || !valid {
			jsonError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}

		if !row.Enabled {
			jsonError(w, http.StatusForbidden, "account disabled")
			return
		}

		sessionID, err := generateSessionID()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to generate session: "+err.Error())
			return
		}
		expiresAt := time.Now().UTC().Add(sessionExpiryDays * 24 * time.Hour)

		if _, err := db.ExecContext(r.Context(),
			"INSERT INTO session (id, user_id, expires_at) VALUES (?, ?, ?)",
			sessionID, row.ID, expiresAt,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		slog.InfoContext(r.Context(), "user logged in", "username", body.Username, "session_prefix", sessionID[:8])

		jsonOK(w, map[string]any{
			"session_id": sessionID,
			"user": authUser{
				ID:          row.ID,
				Username:    row.Username,
				Displayname: row.Displayname,
				Role:        row.Role,
				Enabled:     row.Enabled,
			},
		})
	}
}

// ValidateSession handles GET /v1/api/admin/auth/validate
// Reads the session ID from the X-Session-ID header.
func ValidateSession(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.Header.Get(sessionHeader)
		if sessionID == "" {
			jsonError(w, http.StatusUnauthorized, "missing "+sessionHeader+" header")
			return
		}

		var row struct {
			ID          string          `db:"id"`
			Username    string          `db:"username"`
			Displayname string          `db:"displayname"`
			Role        models.UserRole `db:"role"`
			Enabled     bool            `db:"enabled"`
			ExpiresAt   time.Time       `db:"expires_at"`
		}
		err := db.GetContext(r.Context(), &row,
			`SELECT u.id, u.username, u.displayname, u.role, u.enabled, s.expires_at
			 FROM session s
			 JOIN user  u ON u.id = s.user_id
			 WHERE s.id = ? LIMIT 1`,
			sessionID,
		)
		if err != nil {
			slog.ErrorContext(r.Context(), "session validation db error", "err", err)
			jsonError(w, http.StatusInternalServerError, "internal server error")
			return
		}

		if time.Now().UTC().After(row.ExpiresAt) {
			_, _ = db.ExecContext(r.Context(), "DELETE FROM session WHERE id = ?", sessionID)
			jsonError(w, http.StatusUnauthorized, "session expired")
			return
		}

		if !row.Enabled {
			jsonError(w, http.StatusForbidden, "account disabled")
			return
		}

		jsonOK(w, map[string]any{
			"success": true,
			"user": authUser{
				ID:          row.ID,
				Username:    row.Username,
				Displayname: row.Displayname,
				Role:        row.Role,
				Enabled:     row.Enabled,
			},
		})
	}
}

// LogoutSession handles POST /v1/api/admin/auth/logout
// Reads the session ID from the X-Session-ID header.
func LogoutSession(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.Header.Get(sessionHeader)
		if sessionID == "" {
			jsonError(w, http.StatusBadRequest, "missing "+sessionHeader+" header")
			return
		}

		if _, err := db.ExecContext(r.Context(),
			"DELETE FROM session WHERE id = ?", sessionID,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		slog.InfoContext(r.Context(), "session logged out", "session_prefix", sessionID[:8])

		jsonOK(w, map[string]bool{"logged_out": true})
	}
}
