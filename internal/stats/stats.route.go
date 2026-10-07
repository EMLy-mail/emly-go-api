package stats

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/config"
	"emly-api-go/internal/dbvalue"
	"emly-api-go/internal/models"
	"emly-api-go/internal/presencehub"
	"emly-api-go/internal/productreg"
	"emly-api-go/internal/response"
	"emly-api-go/internal/session"
	"emly-api-go/internal/ttlcache"
	"emly-api-go/internal/updaterclient"
)

var validEventBuckets = map[string]bool{"day": true, "hour": true}

const defaultConnectedWindowMinutes = 15

// maxConnectedWindowMinutes caps ?window_minutes=. A week is already far past
// any useful reading of "currently connected", and the cap keeps the summary
// cache's key space small - the param is part of its key.
const maxConnectedWindowMinutes = 7 * 24 * 60

// eventProductFilter reads the optional ?product= query param and validates
// it (see validProduct). It defaults to "emly" so existing dashboards keep
// counting EMLy client traffic only, unaffected by the updater's own
// self-update checks; pass product=updater for those, product=<slug> for any
// other registered product, or product=all for everything.
func eventProductFilter(r *http.Request, reg *productreg.Registry) (product string, ok bool) {
	return validProduct(reg, r.URL.Query().Get("product"))
}

// validProduct is eventProductFilter's value-only counterpart, shared with the
// WS subscribe path (stats_stream.route.go), which has no *http.Request to
// read a query param from. Valid values are every product in the registry
// (disabled ones too: their history is still data), "updater" (the Agent's
// own self-update traffic) and "all".
func validProduct(reg *productreg.Registry, product string) (resolved string, ok bool) {
	if product == "" {
		product = productreg.EMLy
	}
	if product == "all" || product == updaterclient.ProductUpdater || reg.Has(product) {
		return product, true
	}
	return product, false
}

// productError is the 400 message for an invalid product, listing what the
// registry accepts right now.
func productError(reg *productreg.Registry) string {
	valid := []string{}
	for _, p := range reg.List() {
		valid = append(valid, p.Slug)
	}
	valid = append(valid, updaterclient.ProductUpdater, "all")
	return "product must be one of: " + strings.Join(valid, ", ")
}

// productFilter returns the SQL fragment + arg constraining
// updater_events/updater_event_hourly (they share the product column, so one
// filter serves both) to an already-validated product. For a restricted
// session scope, "all" means "all of the products assigned to me", plus the
// Agent's own self-update traffic, which belongs to no product.
func productFilter(product string, scope session.Scope) (clause string, args []interface{}) {
	if product != "all" {
		return " AND product = ?", []interface{}{product}
	}
	if !scope.IsRestricted() {
		return "", nil
	}
	allowed := append(scope.Products(), updaterclient.ProductUpdater)
	return " AND product IN (" + placeholders(len(allowed)) + ")", stringArgs(allowed)
}

// productAllowed reports whether scope may read product's stats. "all" is
// always allowed - productFilter narrows it - and so is "updater", the Agent's
// self-update traffic, which no product owns.
func productAllowed(scope session.Scope, product string) bool {
	return product == "all" || product == updaterclient.ProductUpdater || scope.Allows(product)
}

// clientScopeClause constrains updater_clients to the machines scope may see:
// those with at least one assigned product installed (updater_client_products),
// plus, for a scope that sees them (admins), those with no product installed.
// Unrestricted scopes get no clause; a scope with no products and no
// unassigned machines sees no machine. The clause is a bare condition, to be
// joined with WHERE or AND.
func clientScopeClause(scope session.Scope) (clause string, args []interface{}) {
	if !scope.IsRestricted() {
		return "", nil
	}
	const unassigned = "NOT EXISTS (SELECT 1 FROM updater_client_products cp WHERE cp.client_id = updater_clients.id)"
	products := scope.Products()
	if len(products) == 0 {
		if scope.SeesUnassignedClients() {
			return unassigned, nil
		}
		return "0 = 1", nil
	}
	assigned := "EXISTS (SELECT 1 FROM updater_client_products cp WHERE cp.client_id = updater_clients.id AND cp.product IN (" +
		placeholders(len(products)) + "))"
	if scope.SeesUnassignedClients() {
		return "(" + assigned + " OR " + unassigned + ")", stringArgs(products)
	}
	return assigned, stringArgs(products)
}

// whereClientScope renders clientScopeClause as a complete WHERE clause
// (leading space included), or "" when there is nothing to constrain.
func whereClientScope(scope session.Scope) (string, []interface{}) {
	clause, args := clientScopeClause(scope)
	if clause == "" {
		return "", nil
	}
	return " WHERE " + clause, args
}

// clientInScope reports whether scope may see client id.
func clientInScope(ctx context.Context, db *sqlx.DB, scope session.Scope, id int64) (bool, error) {
	clause, args := clientScopeClause(scope)
	if clause == "" {
		return true, nil
	}
	var n int
	err := db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM updater_clients WHERE id = ? AND `+clause, append([]interface{}{id}, args...)...)
	return n > 0, err
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func stringArgs(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
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
//
// Every client aggregate is limited to the machines scope may see
// (clientScopeClause), so a scoped dashboard's totals add up to its own list.
func fetchStatsSummary(ctx context.Context, db *sqlx.DB, windowMinutes int, product string, scope session.Scope) (StatsSummary, error) {
	productClause, productArgs := productFilter(product, scope)
	whereScope, scopeArgs := whereClientScope(scope)
	andScope, _ := clientScopeClause(scope)
	if andScope != "" {
		andScope = " AND " + andScope
	}

	var summary StatsSummary
	summary.WindowMinutes = windowMinutes
	summary.Product = product

	if err := db.GetContext(ctx, &summary.TotalClients, `SELECT COUNT(*) FROM updater_clients`+whereScope, scopeArgs...); err != nil {
		return summary, err
	}

	if err := db.GetContext(ctx, &summary.ConnectedClients,
		`SELECT COUNT(*) FROM updater_clients WHERE last_seen_at >= NOW() - INTERVAL ? MINUTE`+andScope,
		append([]interface{}{windowMinutes}, scopeArgs...)...,
	); err != nil {
		return summary, err
	}

	// Summed from the updater_event_hourly rollup (migration 20), not from
	// updater_events: the raw form of this query re-scanned every event of the
	// last day, and it runs again on each ingested event for each open
	// /v2/stats/stream connection. Here it reads 24 hours x one row per
	// (product, event_type) - a few dozen rows.
	//
	// The window is hour-aligned as a result: it covers the 24 whole hours
	// before the current one plus the current partial hour, so the figure can
	// include up to an hour more than a rolling exact 24h. It is a dashboard
	// volume indicator, and rounding it up is the harmless direction.
	if err := db.SelectContext(ctx, &summary.EventsLast24h,
		`SELECT event_type, CAST(SUM(count) AS UNSIGNED) AS count FROM updater_event_hourly
		 WHERE bucket_hour >= DATE_FORMAT(NOW() - INTERVAL 24 HOUR, `+updaterclient.HourBucketExpr+`)`+productClause+`
		 GROUP BY event_type`,
		productArgs...,
	); err != nil {
		return summary, err
	}

	if err := db.SelectContext(ctx, &summary.ClientsByVersion,
		`SELECT updater_version, COUNT(*) AS count FROM updater_clients`+whereScope+` GROUP BY updater_version`,
		scopeArgs...,
	); err != nil {
		return summary, err
	}

	// clients_by_config_revision answers "has the fleet picked up
	// revision N yet" from the same client rows GET /v2/config already
	// updates on every fetch (API design doc §8) - no new telemetry.
	if err := db.SelectContext(ctx, &summary.ClientsByConfigRevision,
		`SELECT config_revision, COUNT(*) AS count FROM updater_clients`+whereScope+` GROUP BY config_revision`,
		scopeArgs...,
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
func GetStatsSummary(db *sqlx.DB, cfg *config.Config, reg *productreg.Registry) http.HandlerFunc {
	cache := ttlcache.New[StatsSummary](cfg.StatsCacheTTL)

	return func(w http.ResponseWriter, r *http.Request) {
		windowMinutes := defaultConnectedWindowMinutes
		if wm := r.URL.Query().Get("window_minutes"); wm != "" {
			if v, err := strconv.Atoi(wm); err == nil && v > 0 {
				windowMinutes = min(v, maxConnectedWindowMinutes)
			}
		}

		product, ok := eventProductFilter(r, reg)
		if !ok {
			response.Error(w, http.StatusBadRequest, productError(reg))
			return
		}
		scope := session.ScopeFrom(r.Context())
		if !productAllowed(scope, product) {
			response.Error(w, http.StatusForbidden, "product not assigned to this user")
			return
		}

		summary, err := cache.Get(product+"|"+strconv.Itoa(windowMinutes)+"|"+scope.Key(), func() (StatsSummary, error) {
			return fetchStatsSummary(r.Context(), db, windowMinutes, product, scope)
		})
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch stats summary")
			return
		}

		// Let the browser's own cache absorb the polling too, so a dashboard
		// left open doesn't even reach the process between refreshes. Private
		// because the response is admin-key gated and must not be held by a
		// shared proxy.
		if cfg.StatsCacheTTL > 0 {
			w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(int(cfg.StatsCacheTTL.Seconds())))
		}
		response.OK(w, summary)
	}
}

// fetchStatsClientsPage backs the paginated GET /v2/stats/clients.
func fetchStatsClientsPage(ctx context.Context, db *sqlx.DB, page, pageSize int, onlyOnline bool, windowMinutes int, scope session.Scope) (clients []models.UpdaterClient, total int, err error) {
	offset := (page - 1) * pageSize

	var conds []string
	var whereArgs []interface{}
	if onlyOnline {
		conds = append(conds, "last_seen_at >= NOW() - INTERVAL ? MINUTE")
		whereArgs = append(whereArgs, windowMinutes)
	}
	if clause, args := clientScopeClause(scope); clause != "" {
		conds = append(conds, clause)
		whereArgs = append(whereArgs, args...)
	}
	whereClause := ""
	if len(conds) > 0 {
		whereClause = "WHERE " + strings.Join(conds, " AND ")
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
	if err = attachProducts(ctx, db, clients); err != nil {
		return nil, 0, err
	}

	return clients, total, nil
}

// fetchAllStatsClients backs the stats:clients WS channel's snapshot: every
// known client, unpaginated. The fleet this serves is a few hundred rows at
// most (design doc §1/§5.2), and the channel intentionally carries no
// server-side online/window filter - see the design doc's implementation
// notes for why.
func fetchAllStatsClients(ctx context.Context, db *sqlx.DB, scope session.Scope) ([]models.UpdaterClient, error) {
	var clients []models.UpdaterClient
	where, args := whereClientScope(scope)
	if err := db.SelectContext(ctx, &clients, `SELECT * FROM updater_clients`+where+` ORDER BY last_seen_at DESC`, args...); err != nil {
		return nil, err
	}
	if err := attachProducts(ctx, db, clients); err != nil {
		return nil, err
	}
	return clients, nil
}

// attachProducts sets Products on each client to its installed-products
// inventory (updater_client_products), in one query for the whole slice, so
// the dashboard's client list can show it without a detail call per row.
// Every installed product is listed, not only the session's assigned ones -
// the same inventory GET /clients/{id} returns. A client with nothing
// installed gets an empty, non-nil slice, so the field reads "none" rather
// than "not reported".
func attachProducts(ctx context.Context, db *sqlx.DB, clients []models.UpdaterClient) error {
	if len(clients) == 0 {
		return nil
	}
	ids := make([]interface{}, len(clients))
	byID := make(map[int]*models.UpdaterClient, len(clients))
	for i := range clients {
		ids[i] = clients[i].ID
		clients[i].Products = []models.ClientProduct{}
		byID[clients[i].ID] = &clients[i]
	}
	var rows []struct {
		ClientID int `db:"client_id"`
		models.ClientProduct
	}
	if err := db.SelectContext(ctx, &rows,
		`SELECT client_id, product, version, updated_at FROM updater_client_products WHERE client_id IN (`+placeholders(len(ids))+`) ORDER BY product`,
		ids...,
	); err != nil {
		return err
	}
	for _, row := range rows {
		if c, ok := byID[row.ClientID]; ok {
			c.Products = append(c.Products, row.ClientProduct)
		}
	}
	return nil
}

// decorateOnline sets Online on each client from presence (internal/
// presencehub), the in-memory GET /v2/client/ws connection registry (design
// doc §5). It is a response-time decoration, not a DB column. Every stats
// handler that returns a client (ListStatsClients, GetStatsClientDetail, the
// stats:clients WS channel) decorates it - presence being nil (tests, or a
// build that never constructs one) decorates every client as offline rather
// than panicking, never leaves the field simply unset.
func decorateOnline(clients []models.UpdaterClient, presence *presencehub.Hub) {
	for i := range clients {
		clients[i].Online = presence.Online(int64(clients[i].ID))
	}
}

// ListStatsClients returns a paginated list of known EMLy Updater clients,
// each decorated with its current online state from presence
// (internal/presencehub).
func ListStatsClients(db *sqlx.DB, presence *presencehub.Hub) http.HandlerFunc {
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

		clients, total, err := fetchStatsClientsPage(r.Context(), db, page, pageSize, onlyOnline, windowMinutes, session.ScopeFrom(r.Context()))
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch clients")
			return
		}
		decorateOnline(clients, presence)

		response.OK(w, map[string]interface{}{
			"data":        clients,
			"total":       total,
			"page":        page,
			"page_size":   pageSize,
			"total_pages": int(math.Ceil(float64(total) / float64(pageSize))),
		})
	}
}

// GetStatsClientDetail returns one client, its recent event history and the
// version of each product it has installed, the
// client decorated with its current online state from presence
// (internal/presencehub) exactly like ListStatsClients - see decorateOnline.
func GetStatsClientDetail(db *sqlx.DB, presence *presencehub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(chi.URLParam(r, "id"))
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid client id")
			return
		}
		// A machine outside the session's scope is a 404, same as one that
		// does not exist.
		if ok, err := clientInScope(r.Context(), db, session.ScopeFrom(r.Context()), int64(id)); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch client")
			return
		} else if !ok {
			response.Error(w, http.StatusNotFound, "client not found")
			return
		}

		var client models.UpdaterClient
		if err := db.GetContext(r.Context(), &client, `SELECT * FROM updater_clients WHERE id = ?`, id); err != nil {
			response.Error(w, http.StatusNotFound, "client not found")
			return
		}
		// Same decorateOnline ListStatsClients uses, on a one-element slice -
		// one code path for "does this client show up online", not two that
		// can drift (see TestDecorateOnline_SingleClientSlice).
		single := []models.UpdaterClient{client}
		decorateOnline(single, presence)
		client = single[0]

		var events []models.UpdaterEvent
		if err := db.SelectContext(r.Context(), &events,
			`SELECT * FROM updater_events WHERE client_id = ? ORDER BY created_at DESC LIMIT 50`, id,
		); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch client events")
			return
		}

		// Installed version per product (updater_client_products): the
		// per-product generalization of client.emly_version.
		products := []models.ClientProduct{}
		if err := db.SelectContext(r.Context(), &products,
			`SELECT product, version, updated_at FROM updater_client_products WHERE client_id = ? ORDER BY product`, id,
		); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch client products")
			return
		}

		response.OK(w, map[string]interface{}{
			"client":   client,
			"events":   events,
			"products": products,
		})
	}
}

// DeleteStatsClient handles DELETE /v2/stats/clients/{id}: it removes one
// updater_clients row together with its whole event history. The events go
// first, by an explicit DELETE, and the client after, both inside one
// transaction - so a failure half-way leaves the client exactly as it was
// rather than a client with part of its history missing. The ON DELETE
// CASCADE on updater_events.client_id would take the rows anyway; deleting
// them by hand is what gives the response an exact events_deleted (the
// statement's own RowsAffected, not a COUNT(*) that a concurrent insert can
// outdate) and keeps the operation correct even on a database where the
// foreign key was never created.
//
// Neither delete touches updater_event_hourly: the fleet-wide charts keep
// counting the traffic this machine produced while it existed. That is
// deliberate - a dashboard's history should not rewrite itself because an
// operator tidied up a decommissioned box - and it is the same reason the
// retention pruner leaves the rollup alone (see internal/eventprune).
//
// Deleting a client is not a way to keep a machine out. The next request
// carrying its headers recreates the row through updaterclient.Upsert, with
// the counters back at zero - this only forgets what was seen so far, which
// is what makes it useful for clearing a decommissioned machine or a test
// rig out of the dashboard. To actually block one, ban its HWID
// (internal/middleware/ban.go), which survives renames and IP changes.
func DeleteStatsClient(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid client id")
			return
		}
		if ok, err := clientInScope(r.Context(), db, session.ScopeFrom(r.Context()), id); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch client")
			return
		} else if !ok {
			response.Error(w, http.StatusNotFound, "client not found")
			return
		}

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to delete client")
			return
		}
		defer tx.Rollback()

		// Read (and lock) the row first: it turns "no such client" into a
		// clean 404, keeps a concurrent Upsert from writing new events for it
		// between the two deletes, and the identity is what makes the audit
		// line worth reading once the row itself is gone.
		var client models.UpdaterClient
		err = tx.GetContext(r.Context(), &client, `SELECT * FROM updater_clients WHERE id = ? FOR UPDATE`, id)
		if errors.Is(err, sql.ErrNoRows) {
			response.Error(w, http.StatusNotFound, "client not found")
			return
		}
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch client")
			return
		}

		res, err := tx.ExecContext(r.Context(), `DELETE FROM updater_events WHERE client_id = ?`, id)
		if err != nil {
			slog.ErrorContext(r.Context(), "failed to delete client events", "client_id", id, "error", err)
			response.Error(w, http.StatusInternalServerError, "failed to delete client events")
			return
		}
		eventsDeleted, err := res.RowsAffected()
		if err != nil {
			// The count is for the report, not for the delete.
			eventsDeleted = -1
		}

		// Same reasoning as the events: the FK would cascade, the explicit
		// delete keeps it correct where the FK was never created.
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM updater_client_products WHERE client_id = ?`, id); err != nil {
			slog.ErrorContext(r.Context(), "failed to delete client products", "client_id", id, "error", err)
			response.Error(w, http.StatusInternalServerError, "failed to delete client products")
			return
		}

		if _, err := tx.ExecContext(r.Context(), `DELETE FROM updater_clients WHERE id = ?`, id); err != nil {
			slog.ErrorContext(r.Context(), "failed to delete client", "client_id", id, "error", err)
			response.Error(w, http.StatusInternalServerError, "failed to delete client")
			return
		}

		if err := tx.Commit(); err != nil {
			slog.ErrorContext(r.Context(), "failed to commit client delete", "client_id", id, "error", err)
			response.Error(w, http.StatusInternalServerError, "failed to delete client")
			return
		}

		slog.WarnContext(r.Context(), "updater client deleted",
			"client_id", client.ID,
			"hwid", dbvalue.Deref(client.HWID),
			"hostname", client.Hostname,
			"events_deleted", eventsDeleted)

		response.OK(w, map[string]interface{}{
			"status":         "deleted",
			"client_id":      client.ID,
			"events_deleted": eventsDeleted,
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
//
// Served from the updater_event_hourly rollup (migration 20). Against the raw
// table this was the single most expensive query in the API: grouping by
// DATE(created_at) cannot use an index, so a default 30-day window meant
// scanning the range into a temporary table and sorting it - and the WS
// stream re-ran it per ingested event, per connection. Summing hourly rows
// instead reads 720 of them for the same 30 days.
//
// One behavioural consequence: hour is the finest bucket the API offers, so
// the from/to filter now has hourly granularity. from is floored to the top
// of its hour, meaning a range starting mid-hour includes that whole hour -
// the right direction for a daily chart, which otherwise shows a clipped
// first column.
func fetchStatsEvents(ctx context.Context, db *sqlx.DB, bucket, eventType, product string, scope session.Scope, from, to time.Time) (StatsEventsResponse, error) {
	productClause, productArgs := productFilter(product, scope)

	bucketExpr := "DATE(bucket_hour)"
	if bucket == "hour" {
		bucketExpr = `DATE_FORMAT(bucket_hour, ` + updaterclient.HourBucketExpr + `)`
	}

	query := `SELECT ` + bucketExpr + ` AS bucket, event_type, CAST(SUM(count) AS UNSIGNED) AS count
	          FROM updater_event_hourly
	          WHERE bucket_hour BETWEEN ? AND ?`
	args := []interface{}{from.Truncate(time.Hour), to}
	query += productClause
	args = append(args, productArgs...)
	if eventType != "" {
		query += ` AND event_type = ?`
		args = append(args, eventType)
	}
	query += ` GROUP BY bucket, event_type ORDER BY bucket ASC`

	// From/To report the window as the caller asked for it, not the floored
	// bound the query used: the response describes the request, and a client
	// echoing it back must not drift an hour earlier on every round trip.
	resp := StatsEventsResponse{Bucket: bucket, Product: product, From: from, To: to}
	err := db.SelectContext(ctx, &resp.Data, query, args...)
	return resp, err
}

// GetStatsEvents returns time-bucketed event counts for dashboard charts.
//
// Memoized for cfg.StatsCacheTTL like GetStatsSummary, and for the same
// reason: a chart that plots whole days is polled far faster than a day-long
// bucket can move. The cache key includes every param that shapes the query,
// with the timestamps rounded down to a TTL-wide step - without that, the
// default window ends at time.Now() and no two requests would ever share a
// key. Two callers whose windows differ by less than the TTL therefore share
// one payload, which is the staleness the TTL already licenses.
func GetStatsEvents(db *sqlx.DB, cfg *config.Config, reg *productreg.Registry) http.HandlerFunc {
	cache := ttlcache.New[StatsEventsResponse](cfg.StatsCacheTTL)

	return func(w http.ResponseWriter, r *http.Request) {
		bucket := r.URL.Query().Get("bucket")
		if bucket == "" {
			bucket = "day"
		}
		if !validEventBuckets[bucket] {
			response.Error(w, http.StatusBadRequest, "bucket must be one of: day, hour")
			return
		}

		eventType := r.URL.Query().Get("event_type")

		product, ok := eventProductFilter(r, reg)
		if !ok {
			response.Error(w, http.StatusBadRequest, productError(reg))
			return
		}
		scope := session.ScopeFrom(r.Context())
		if !productAllowed(scope, product) {
			response.Error(w, http.StatusForbidden, "product not assigned to this user")
			return
		}

		from := time.Now().UTC().AddDate(0, 0, -30)
		if f := r.URL.Query().Get("from"); f != "" {
			if t, err := time.Parse(time.RFC3339, f); err == nil {
				from = t
			} else {
				response.Error(w, http.StatusBadRequest, "from must be RFC3339")
				return
			}
		}

		to := time.Now().UTC()
		if t := r.URL.Query().Get("to"); t != "" {
			if parsed, err := time.Parse(time.RFC3339, t); err == nil {
				to = parsed
			} else {
				response.Error(w, http.StatusBadRequest, "to must be RFC3339")
				return
			}
		}

		key := strings.Join([]string{
			bucket, product, eventType, scope.Key(),
			quantize(from, cfg.StatsCacheTTL).Format(time.RFC3339),
			quantize(to, cfg.StatsCacheTTL).Format(time.RFC3339),
		}, "|")

		resp, err := cache.Get(key, func() (StatsEventsResponse, error) {
			return fetchStatsEvents(r.Context(), db, bucket, eventType, product, scope, from, to)
		})
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch events")
			return
		}

		// Same private browser caching as /summary: a dashboard left open
		// stops reaching the process at all between refreshes.
		if cfg.StatsCacheTTL > 0 {
			w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(int(cfg.StatsCacheTTL.Seconds())))
		}
		response.OK(w, resp)
	}
}

// quantize rounds t down to a multiple of step, so timestamps that differ by
// less than step collapse onto one cache key. A non-positive step (caching
// disabled) returns t untouched.
func quantize(t time.Time, step time.Duration) time.Time {
	if step <= 0 {
		return t
	}
	return t.Truncate(step)
}
