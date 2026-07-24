package handlers

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
)

var validEventBuckets = map[string]bool{"day": true, "hour": true}

const defaultConnectedWindowMinutes = 15

// GetStatsSummary returns fleet-wide EMLy Updater stats: total/connected
// client counts, event volume over the last 24h, and version adoption.
func GetStatsSummary(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		windowMinutes := defaultConnectedWindowMinutes
		if wm := r.URL.Query().Get("window_minutes"); wm != "" {
			if v, err := strconv.Atoi(wm); err == nil && v > 0 {
				windowMinutes = v
			}
		}

		var totalClients int
		if err := db.GetContext(r.Context(), &totalClients, `SELECT COUNT(*) FROM updater_clients`); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to count clients")
			return
		}

		var connectedClients int
		if err := db.GetContext(r.Context(), &connectedClients,
			`SELECT COUNT(*) FROM updater_clients WHERE last_seen_at >= NOW() - INTERVAL ? MINUTE`,
			windowMinutes,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to count connected clients")
			return
		}

		type eventCount struct {
			EventType string `db:"event_type" json:"event_type"`
			Count     int    `db:"count"      json:"count"`
		}
		var eventsLast24h []eventCount
		if err := db.SelectContext(r.Context(), &eventsLast24h,
			`SELECT event_type, COUNT(*) AS count FROM updater_events
			 WHERE created_at >= NOW() - INTERVAL 24 HOUR
			 GROUP BY event_type`,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to count events")
			return
		}

		type versionCount struct {
			UpdaterVersion *string `db:"updater_version" json:"updater_version"`
			Count          int     `db:"count"           json:"count"`
		}
		var clientsByVersion []versionCount
		if err := db.SelectContext(r.Context(), &clientsByVersion,
			`SELECT updater_version, COUNT(*) AS count FROM updater_clients GROUP BY updater_version`,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to group clients by version")
			return
		}

		jsonOK(w, map[string]interface{}{
			"total_clients":      totalClients,
			"connected_clients":  connectedClients,
			"window_minutes":     windowMinutes,
			"events_last_24h":    eventsLast24h,
			"clients_by_version": clientsByVersion,
		})
	}
}

// ListStatsClients returns a paginated list of known EMLy Updater clients.
func ListStatsClients(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, pageSize := 1, 20
		if p := r.URL.Query().Get("page"); p != "" {
			if v, err := strconv.Atoi(p); err == nil && v > 0 {
				page = v
			}
		}
		if ps := r.URL.Query().Get("page_size"); ps != "" {
			if v, err := strconv.Atoi(ps); err == nil && v > 0 && v <= 100 {
				pageSize = v
			}
		}
		offset := (page - 1) * pageSize

		onlyOnline := r.URL.Query().Get("online") == "true"
		windowMinutes := defaultConnectedWindowMinutes
		if wm := r.URL.Query().Get("window_minutes"); wm != "" {
			if v, err := strconv.Atoi(wm); err == nil && v > 0 {
				windowMinutes = v
			}
		}

		whereClause := ""
		var whereArgs []interface{}
		if onlyOnline {
			whereClause = "WHERE last_seen_at >= NOW() - INTERVAL ? MINUTE"
			whereArgs = append(whereArgs, windowMinutes)
		}

		var total int
		countQuery := `SELECT COUNT(*) FROM updater_clients ` + whereClause
		if err := db.GetContext(r.Context(), &total, countQuery, whereArgs...); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to count clients")
			return
		}

		listQuery := `SELECT * FROM updater_clients ` + whereClause + ` ORDER BY last_seen_at DESC LIMIT ? OFFSET ?`
		listArgs := append(whereArgs, pageSize, offset)

		var clients []models.UpdaterClient
		if err := db.SelectContext(r.Context(), &clients, listQuery, listArgs...); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch clients")
			return
		}

		jsonOK(w, map[string]interface{}{
			"data":        clients,
			"total":       total,
			"page":        page,
			"page_size":   pageSize,
			"total_pages": int(math.Ceil(float64(total) / float64(pageSize))),
		})
	}
}

// GetStatsClientDetail returns one client and its recent event history.
func GetStatsClientDetail(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(chi.URLParam(r, "id"))
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid client id")
			return
		}

		var client models.UpdaterClient
		if err := db.GetContext(r.Context(), &client, `SELECT * FROM updater_clients WHERE id = ?`, id); err != nil {
			jsonError(w, http.StatusNotFound, "client not found")
			return
		}

		var events []models.UpdaterEvent
		if err := db.SelectContext(r.Context(), &events,
			`SELECT * FROM updater_events WHERE client_id = ? ORDER BY created_at DESC LIMIT 50`, id,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch client events")
			return
		}

		jsonOK(w, map[string]interface{}{
			"client": client,
			"events": events,
		})
	}
}

// GetStatsEvents returns time-bucketed event counts for dashboard charts.
func GetStatsEvents(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bucket := r.URL.Query().Get("bucket")
		if bucket == "" {
			bucket = "day"
		}
		if !validEventBuckets[bucket] {
			jsonError(w, http.StatusBadRequest, "bucket must be one of: day, hour")
			return
		}

		eventType := r.URL.Query().Get("event_type")

		from := time.Now().UTC().AddDate(0, 0, -30)
		if f := r.URL.Query().Get("from"); f != "" {
			if t, err := time.Parse(time.RFC3339, f); err == nil {
				from = t
			} else {
				jsonError(w, http.StatusBadRequest, "from must be RFC3339")
				return
			}
		}

		to := time.Now().UTC()
		if t := r.URL.Query().Get("to"); t != "" {
			if parsed, err := time.Parse(time.RFC3339, t); err == nil {
				to = parsed
			} else {
				jsonError(w, http.StatusBadRequest, "to must be RFC3339")
				return
			}
		}

		bucketExpr := "DATE(created_at)"
		if bucket == "hour" {
			bucketExpr = `DATE_FORMAT(created_at, '%Y-%m-%d %H:00:00')`
		}

		query := `SELECT ` + bucketExpr + ` AS bucket, event_type, COUNT(*) AS count
		          FROM updater_events
		          WHERE created_at BETWEEN ? AND ?`
		args := []interface{}{from, to}
		if eventType != "" {
			query += ` AND event_type = ?`
			args = append(args, eventType)
		}
		query += ` GROUP BY bucket, event_type ORDER BY bucket ASC`

		type bucketCount struct {
			Bucket    string `db:"bucket"     json:"bucket"`
			EventType string `db:"event_type" json:"event_type"`
			Count     int    `db:"count"      json:"count"`
		}
		var counts []bucketCount
		if err := db.SelectContext(r.Context(), &counts, query, args...); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch events")
			return
		}

		jsonOK(w, map[string]interface{}{
			"bucket": bucket,
			"from":   from,
			"to":     to,
			"data":   counts,
		})
	}
}
