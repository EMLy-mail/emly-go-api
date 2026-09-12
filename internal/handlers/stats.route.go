package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/config"
	"emly-api-go/internal/models"
)

var validEventBuckets = map[string]bool{"day": true, "hour": true}

var validEventProducts = map[string]bool{"emly": true, "updater": true, "all": true}

const defaultConnectedWindowMinutes = 15

// maxConnectedWindowMinutes caps ?window_minutes=. A week is already far past
// any useful reading of "currently connected", and the cap keeps the summary
// cache's key space small - the param is part of its key.
const maxConnectedWindowMinutes = 7 * 24 * 60

// eventProductFilter reads the optional ?product= query param and returns the
// SQL fragment + arg constraining updater_events. It defaults to "emly" so
// existing dashboards keep counting EMLy client traffic only, unaffected by
// the updater's own self-update checks; pass product=updater for those, or
// product=all for both.
func eventProductFilter(r *http.Request) (product, clause string, args []interface{}, ok bool) {
	return productFilter(r.URL.Query().Get("product"))
}

// productFilter is eventProductFilter's value-only counterpart, shared with
// the WS subscribe path (internal/handlers/stats_stream.route.go), which has
// no *http.Request to read a query param from.
func productFilter(product string) (resolved, clause string, args []interface{}, ok bool) {
	if product == "" {
		product = "emly"
	}
	if !validEventProducts[product] {
		return product, "", nil, false
	}
	if product == "all" {
		return product, "", nil, true
	}
	return product, " AND product = ?", []interface{}{product}, true
}

// EventCount is one row of StatsSummary.EventsLast24h.
type EventCount struct {
	EventType string `db:"event_type" json:"event_type"`
	Count     int    `db:"count"      json:"count"`
}

// VersionCount is one row of StatsSummary.ClientsByVersion.
type VersionCount struct {
	UpdaterVersion *string `db:"updater_version" json:"updater_version"`
	Count          int     `db:"count"           json:"count"`
}

// RevisionCount is one row of StatsSummary.ClientsByConfigRevision.
type RevisionCount struct {
	ConfigRevision *int64 `db:"config_revision" json:"config_revision"`
	Count          int    `db:"count"           json:"count"`
}

// StatsSummary is the fleet-wide summary shape shared by GET
// /v2/stats/summary and the stats:summary WS channel (snapshot and update).
type StatsSummary struct {
	TotalClients            int             `json:"total_clients"`
	ConnectedClients        int             `json:"connected_clients"`
	WindowMinutes           int             `json:"window_minutes"`
	Product                 string          `json:"product"`
	EventsLast24h           []EventCount    `json:"events_last_24h"`
	ClientsByVersion        []VersionCount  `json:"clients_by_version"`
	ClientsByConfigRevision []RevisionCount `json:"clients_by_config_revision"`
}

// fetchStatsSummary backs both GET /v2/stats/summary and the stats:summary
// WS channel. product must already be validated (see productFilter).
func fetchStatsSummary(ctx context.Context, db *sqlx.DB, windowMinutes int, product string) (StatsSummary, error) {
	_, productClause, productArgs, _ := productFilter(product)

	var summary StatsSummary
	summary.WindowMinutes = windowMinutes
	summary.Product = product

	if err := db.GetContext(ctx, &summary.TotalClients, `SELECT COUNT(*) FROM updater_clients`); err != nil {
		return summary, err
	}

	if err := db.GetContext(ctx, &summary.ConnectedClients,
		`SELECT COUNT(*) FROM updater_clients WHERE last_seen_at >= NOW() - INTERVAL ? MINUTE`,
		windowMinutes,
	); err != nil {
		return summary, err
	}

	if err := db.SelectContext(ctx, &summary.EventsLast24h,
		`SELECT event_type, COUNT(*) AS count FROM updater_events
		 WHERE created_at >= NOW() - INTERVAL 24 HOUR`+productClause+`
		 GROUP BY event_type`,
		productArgs...,
	); err != nil {
		return summary, err
	}

	if err := db.SelectContext(ctx, &summary.ClientsByVersion,
		`SELECT updater_version, COUNT(*) AS count FROM updater_clients GROUP BY updater_version`,
	); err != nil {
		return summary, err
	}

	// clients_by_config_revision answers "has the fleet picked up
	// revision N yet" from the same client rows GET /v2/config already
	// updates on every fetch (API design doc §8) - no new telemetry.
	if err := db.SelectContext(ctx, &summary.ClientsByConfigRevision,
		`SELECT config_revision, COUNT(*) AS count FROM updater_clients GROUP BY config_revision`,
	); err != nil {
		return summary, err
	}

	return summary, nil
}

// GetStatsSummary returns fleet-wide EMLy Updater stats: total/connected
// client counts, event volume over the last 24h, and version adoption.
//
// The payload is memoized for cfg.StatsCacheTTL, keyed by the two query
// params that shape it. Dashboards poll this endpoint continuously and none
// of these figures - a 24h event total, a version histogram - move on a
// per-request timescale, so serving a slightly stale copy costs the reader
// nothing while sparing the database five queries per poll.
func GetStatsSummary(db *sqlx.DB, cfg *config.Config) http.HandlerFunc {
	cache := newTTLCache[StatsSummary](cfg.StatsCacheTTL)

	return func(w http.ResponseWriter, r *http.Request) {
		windowMinutes := defaultConnectedWindowMinutes
		if wm := r.URL.Query().Get("window_minutes"); wm != "" {
			if v, err := strconv.Atoi(wm); err == nil && v > 0 {
				windowMinutes = min(v, maxConnectedWindowMinutes)
			}
		}

		product, _, _, ok := eventProductFilter(r)
		if !ok {
			jsonError(w, http.StatusBadRequest, "product must be one of: emly, updater, all")
			return
		}

		summary, err := cache.get(product+"|"+strconv.Itoa(windowMinutes), func() (StatsSummary, error) {
			return fetchStatsSummary(r.Context(), db, windowMinutes, product)
		})
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch stats summary")
			return
		}

		// Let the browser's own cache absorb the polling too, so a dashboard
		// left open doesn't even reach the process between refreshes. Private
		// because the response is admin-key gated and must not be held by a
		// shared proxy.
		if cfg.StatsCacheTTL > 0 {
			w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(int(cfg.StatsCacheTTL.Seconds())))
		}
		jsonOK(w, summary)
	}
}

// fetchStatsClientsPage backs the paginated GET /v2/stats/clients.
func fetchStatsClientsPage(ctx context.Context, db *sqlx.DB, page, pageSize int, onlyOnline bool, windowMinutes int) (clients []models.UpdaterClient, total int, err error) {
	offset := (page - 1) * pageSize

	whereClause := ""
	var whereArgs []interface{}
	if onlyOnline {
		whereClause = "WHERE last_seen_at >= NOW() - INTERVAL ? MINUTE"
		whereArgs = append(whereArgs, windowMinutes)
	}

	if err = db.GetContext(ctx, &total, `SELECT COUNT(*) FROM updater_clients `+whereClause, whereArgs...); err != nil {
		return nil, 0, err
	}

	listArgs := append(append([]interface{}{}, whereArgs...), pageSize, offset)
	if err = db.SelectContext(ctx, &clients,
		`SELECT * FROM updater_clients `+whereClause+` ORDER BY last_seen_at DESC LIMIT ? OFFSET ?`,
		listArgs...,
	); err != nil {
		return nil, 0, err
	}

	return clients, total, nil
}

// fetchAllStatsClients backs the stats:clients WS channel's snapshot: every
// known client, unpaginated. The fleet this serves is a few hundred rows at
// most (design doc §1/§5.2), and the channel intentionally carries no
// server-side online/window filter - see the design doc's implementation
// notes for why.
func fetchAllStatsClients(ctx context.Context, db *sqlx.DB) ([]models.UpdaterClient, error) {
	var clients []models.UpdaterClient
	err := db.SelectContext(ctx, &clients, `SELECT * FROM updater_clients ORDER BY last_seen_at DESC`)
	return clients, err
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

		onlyOnline := r.URL.Query().Get("online") == "true"
		windowMinutes := defaultConnectedWindowMinutes
		if wm := r.URL.Query().Get("window_minutes"); wm != "" {
			if v, err := strconv.Atoi(wm); err == nil && v > 0 {
				windowMinutes = v
			}
		}

		clients, total, err := fetchStatsClientsPage(r.Context(), db, page, pageSize, onlyOnline, windowMinutes)
		if err != nil {
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

// DeleteStatsClient removes one row from updater_clients, along with its
// whole event history: updater_events.client_id is declared ON DELETE
// CASCADE, so the rows go with it and the response reports how many did.
//
// Not mounted on any route yet - registerStats does not reference it. Wiring
// it up means adding a DELETE under the admin-key group in
// internal/routes/v2/stats.go, next to GetStatsClientDetail, and documenting
// it in ROUTES.md.
//
// Deleting a client is not a way to keep a machine out. The next request
// carrying its headers recreates the row through upsertUpdaterClient, with
// the counters back at zero - this only forgets what was seen so far, which
// is what makes it useful for clearing a decommissioned machine or a test
// rig out of the dashboard. To actually block one, ban its HWID
// (internal/middleware/ban.go), which survives renames and IP changes.
func DeleteStatsClient(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid client id")
			return
		}

		// Read the row first: it turns "no such client" into a clean 404,
		// and the identity is what makes the audit line worth reading once
		// the row itself is gone.
		var client models.UpdaterClient
		err = db.GetContext(r.Context(), &client, `SELECT * FROM updater_clients WHERE id = ?`, id)
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "client not found")
			return
		}
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch client")
			return
		}

		var eventCount int
		if err := db.GetContext(r.Context(), &eventCount,
			`SELECT COUNT(*) FROM updater_events WHERE client_id = ?`, id,
		); err != nil {
			// The count is for the report, not for the delete - a failure
			// here must not block the operation.
			slog.WarnContext(r.Context(), "failed to count client events before delete",
				"client_id", id, "error", err)
			eventCount = -1
		}

		if _, err := db.ExecContext(r.Context(), `DELETE FROM updater_clients WHERE id = ?`, id); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to delete client")
			return
		}

		slog.WarnContext(r.Context(), "updater client deleted",
			"client_id", client.ID,
			"hwid", derefString(client.HWID),
			"hostname", client.Hostname,
			"events_deleted", eventCount)

		jsonOK(w, map[string]interface{}{
			"status":         "deleted",
			"client_id":      client.ID,
			"events_deleted": eventCount,
		})
	}
}

// StatsEventBucket is one row of StatsEventsResponse.Data.
type StatsEventBucket struct {
	Bucket    string `db:"bucket"     json:"bucket"`
	EventType string `db:"event_type" json:"event_type"`
	Count     int    `db:"count"      json:"count"`
}

// StatsEventsResponse is the time-bucketed event count shape shared by GET
// /v2/stats/events and the stats:events WS channel (snapshot and update).
type StatsEventsResponse struct {
	Bucket  string             `json:"bucket"`
	Product string             `json:"product"`
	From    time.Time          `json:"from"`
	To      time.Time          `json:"to"`
	Data    []StatsEventBucket `json:"data"`
}

// fetchStatsEvents backs both GET /v2/stats/events and the stats:events WS
// channel. bucket and product must already be validated (see
// validEventBuckets / productFilter).
func fetchStatsEvents(ctx context.Context, db *sqlx.DB, bucket, eventType, product string, from, to time.Time) (StatsEventsResponse, error) {
	_, productClause, productArgs, _ := productFilter(product)

	bucketExpr := "DATE(created_at)"
	if bucket == "hour" {
		bucketExpr = `DATE_FORMAT(created_at, '%Y-%m-%d %H:00:00')`
	}

	query := `SELECT ` + bucketExpr + ` AS bucket, event_type, COUNT(*) AS count
	          FROM updater_events
	          WHERE created_at BETWEEN ? AND ?`
	args := []interface{}{from, to}
	query += productClause
	args = append(args, productArgs...)
	if eventType != "" {
		query += ` AND event_type = ?`
		args = append(args, eventType)
	}
	query += ` GROUP BY bucket, event_type ORDER BY bucket ASC`

	resp := StatsEventsResponse{Bucket: bucket, Product: product, From: from, To: to}
	err := db.SelectContext(ctx, &resp.Data, query, args...)
	return resp, err
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

		product, _, _, ok := eventProductFilter(r)
		if !ok {
			jsonError(w, http.StatusBadRequest, "product must be one of: emly, updater, all")
			return
		}

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

		resp, err := fetchStatsEvents(r.Context(), db, bucket, eventType, product, from, to)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to fetch events")
			return
		}

		jsonOK(w, resp)
	}
}
