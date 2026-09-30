package updaterclient

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/dbvalue"
	"emly-api-go/internal/productreg"
)

// InstalledProductsHeader carries the machine's full product inventory as
// comma-separated slug=version pairs, e.g. "emly=3.5.0,foo=1.2.0". Its WS
// counterpart is WSIdentityPayload.InstalledProducts.
const InstalledProductsHeader = "X-EMLy-InstalledProducts"

// maxInstalledProducts bounds how many rows one report can write. A machine
// with more products than this is not something the fleet has; a header that
// claims it is garbage.
const maxInstalledProducts = 32

// installedProductsFromHeader parses InstalledProductsHeader. A request
// without the header returns nil ("not reported"); a header that is present
// but empty returns an empty, non-nil map ("nothing installed") - the same
// "absent != empty" split every other identity field has, except that here
// both answers are expressible on the wire.
func installedProductsFromHeader(h http.Header) map[string]string {
	values, present := h[http.CanonicalHeaderKey(InstalledProductsHeader)]
	if !present {
		return nil
	}
	raw := map[string]string{}
	for _, v := range values {
		for _, pair := range strings.Split(v, ",") {
			slug, version, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if !ok {
				continue
			}
			raw[strings.TrimSpace(slug)] = strings.TrimSpace(version)
		}
	}
	return sanitizeInstalledProducts(raw)
}

// sanitizeInstalledProducts drops entries no row could hold - a malformed
// slug or an empty version - truncates versions to the column width and caps
// the count. nil stays nil (not reported); anything else, even if every entry
// was dropped, stays a non-nil map (a report). Both constructors go through
// it, so a header and a WS identity carrying the same inventory store the
// same rows.
func sanitizeInstalledProducts(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	slugs := make([]string, 0, len(in))
	for slug := range in {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs) // deterministic cap
	out := make(map[string]string, len(in))
	for _, slug := range slugs {
		version := dbvalue.Truncate(strings.TrimSpace(in[slug]), 20)
		if !productreg.ValidSlug(slug) || version == "" {
			continue
		}
		if len(out) == maxInstalledProducts {
			break
		}
		out[slug] = version
	}
	return out
}

// syncInstalledProducts brings updater_client_products in line with what id
// reports for clientID.
//
//   - InstalledProducts nil (not reported): only X-EMLy-AppVersion counts, as
//     the emly row - what updaters that predate the inventory still send.
//     Nothing is deleted.
//   - InstalledProducts non-nil: it is the full inventory. Listed products are
//     upserted, unlisted ones deleted (uninstalled). X-EMLy-AppVersion still
//     fills the emly row when the inventory does not mention emly.
//
// updated_at moves only when a version actually changes (ON UPDATE
// CURRENT_TIMESTAMP skips no-op updates), so it reads as "at this version
// since"; updater_clients.last_seen_at says how current the report is.
//
// A failure is logged, not returned: the inventory is detail on top of the
// sighting, and must not cost the caller its event or its connection.
func syncInstalledProducts(ctx context.Context, db *sqlx.DB, clientID int64, id Identity) {
	versions := make(map[string]string, len(id.InstalledProducts)+1)
	for slug, v := range id.InstalledProducts {
		versions[slug] = v
	}
	if _, ok := versions[productreg.EMLy]; !ok && id.EMLyVersion != "" {
		versions[productreg.EMLy] = id.EMLyVersion
	}

	if len(versions) > 0 {
		var placeholders []string
		var args []interface{}
		for slug, v := range versions {
			placeholders = append(placeholders, "(?, ?, ?)")
			args = append(args, clientID, slug, v)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO updater_client_products (client_id, product, version) VALUES `+strings.Join(placeholders, ", ")+`
			 ON DUPLICATE KEY UPDATE version = VALUES(version)`, args...); err != nil {
			slog.WarnContext(ctx, "updater stats: failed to record installed products", "client_id", clientID, "error", err)
			return
		}
	}

	if id.InstalledProducts == nil {
		return
	}
	query := `DELETE FROM updater_client_products WHERE client_id = ?`
	args := []interface{}{clientID}
	if len(versions) > 0 {
		query += ` AND product NOT IN (?)`
		keys := make([]string, 0, len(versions))
		for slug := range versions {
			keys = append(keys, slug)
		}
		var err error
		query, args, err = sqlx.In(query, clientID, keys)
		if err != nil {
			slog.WarnContext(ctx, "updater stats: failed to build installed products cleanup", "error", err)
			return
		}
	}
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		slog.WarnContext(ctx, "updater stats: failed to prune installed products", "client_id", clientID, "error", err)
	}
}
