package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
)

const banSelectCols = `id, ban_type, value, reason, created_by, created_at`

// ListBans handles GET /v2/bans.
func ListBans(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var bans []models.Ban
		if err := db.SelectContext(r.Context(), &bans,
			`SELECT `+banSelectCols+` FROM bans ORDER BY created_at DESC`); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to list bans")
			return
		}
		if bans == nil {
			bans = []models.Ban{}
		}
		jsonOK(w, bans)
	}
}

// CreateBan handles POST /v2/bans.
//
// Re-banning something already banned is a 200 with the existing row, not a
// 409: the caller asked for a state ("this machine is blocked") that already
// holds, and two operators reacting to the same incident should not have to
// care which of them got there first.
func CreateBan(db *sqlx.DB, bans BanReloader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			BanType models.BanType `json:"ban_type"`
			Value   string         `json:"value"`
			Reason  string         `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		if !models.ValidBanType(body.BanType) {
			jsonError(w, http.StatusBadRequest, "ban_type must be 'ip', 'hwid' or 'hostname'")
			return
		}
		value, err := normalizeBanValue(body.BanType, body.Value)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}

		reason := nullableString(strings.TrimSpace(body.Reason))
		createdBy := sessionUsername(r, db)

		res, err := db.ExecContext(r.Context(),
			`INSERT INTO bans (ban_type, value, reason, created_by) VALUES (?, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`,
			string(body.BanType), value, reason, createdBy,
		)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to create ban")
			return
		}
		id, _ := res.LastInsertId()

		var ban models.Ban
		if err := db.GetContext(r.Context(), &ban,
			`SELECT `+banSelectCols+` FROM bans WHERE id = ?`, id); err != nil {
			jsonError(w, http.StatusInternalServerError, "ban stored but could not be read back")
			return
		}

		reloadBans(r, bans)
		slog.WarnContext(r.Context(), "ban created",
			"ban_id", ban.ID, "ban_type", string(ban.BanType), "ban_value", ban.Value,
			"created_by", derefString(ban.CreatedBy))

		// 201 only when this call is what created the row; a repeat is a 200.
		if n, _ := res.RowsAffected(); n == 1 {
			jsonCreated(w, ban)
			return
		}
		jsonOK(w, ban)
	}
}

// DeleteBan handles DELETE /v2/bans/{id}.
func DeleteBan(db *sqlx.DB, bans BanReloader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid ban id")
			return
		}

		var ban models.Ban
		err = db.GetContext(r.Context(), &ban, `SELECT `+banSelectCols+` FROM bans WHERE id = ?`, id)
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "ban not found")
			return
		}
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch ban")
			return
		}

		if _, err := db.ExecContext(r.Context(), `DELETE FROM bans WHERE id = ?`, id); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to delete ban")
			return
		}

		reloadBans(r, bans)
		slog.WarnContext(r.Context(), "ban removed",
			"ban_id", ban.ID, "ban_type", string(ban.BanType), "ban_value", ban.Value)

		jsonOK(w, map[string]string{"status": "deleted"})
	}
}

// BanReloader refreshes the in-memory ban snapshot after a write.
// *middleware.BanList satisfies it structurally, which is the point: the
// handlers stay decoupled from that package, and a test can pass a stub.
type BanReloader interface {
	Reload(ctx context.Context) error
}

func reloadBans(r *http.Request, bans BanReloader) {
	if bans == nil {
		return
	}
	if err := bans.Reload(r.Context()); err != nil {
		// Not fatal: the ticker in BanList.Run will pick the change up on
		// its next pass, so this delays enforcement rather than losing it.
		slog.WarnContext(r.Context(), "ban list: reload after write failed", "error", err)
	}
}

// normalizeBanValue validates and canonicalises the value for its type, so
// what the matcher compares against is what the operator meant.
//
// An IP is parsed and re-rendered: "10.0.0.1 " and an IPv6 written in a
// different but equivalent form must not become two rows that each fail to
// match the traffic. Hostnames are lower-cased, which is also what makes the
// table's unique key case-insensitive for them. A HWID is taken as typed -
// it is an opaque firmware string, and folding its case would break the
// exact match the header needs.
func normalizeBanValue(t models.BanType, raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", errors.New("value is required")
	}
	if len(v) > 255 {
		return "", errors.New("value is longer than 255 characters")
	}

	switch t {
	case models.BanTypeIP:
		ip := net.ParseIP(v)
		if ip == nil {
			return "", errors.New("value is not a valid IP address")
		}
		return ip.String(), nil
	case models.BanTypeHostname:
		return strings.ToLower(v), nil
	default:
		return v, nil
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
