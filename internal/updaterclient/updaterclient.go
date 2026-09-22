// Package updaterclient owns the EMLy Updater's identity: who a request says
// it comes from, the updater_clients row that identity keys, and the
// updater_events + updater_event_hourly rows a sighting writes.
//
// It exists as its own package because that identity is built from two
// completely different wire formats - the X-EMLy-* headers on every ordinary
// request, and the "identity" JSON message on GET /v2/client/ws - and the two
// must carry the same fields. A field added to one constructor and forgotten in
// the other fills in for updaters that use that path and silently stays NULL
// for the other. Keeping both constructors in one file, feeding one Upsert, is
// what makes that rule visible instead of a comment someone has to remember:
// IdentityFromRequest and IdentityFromWSPayload sit next to each other, and
// every field either adds to both or to neither.
//
// Every feature that serves an updater imports this (updates, configapi,
// clientws), and it imports no feature of its own.
package updaterclient

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/dbvalue"
	"emly-api-go/internal/models"
	"emly-api-go/internal/statshub"
)

// HourBucketExpr renders a DATETIME down to the top of its hour, the
// expression updater_event_hourly.bucket_hour is keyed by. It is the same
// literal format migration 20 backfilled with and the same one the raw-row
// queries used to group by, so buckets stay where they have always been -
// change it in one place and the other two stop lining up.
//
// It is exported because the write side lives here (insertEvent) and the read
// side lives in internal/stats: one definition, two users, no drift.
const HourBucketExpr = `'%Y-%m-%d %H:00:00'`

// Values for updater_events.product: which piece of software an event is
// about. The EMLy Updater checks for EMLy releases and, separately, for its
// own new builds - both from the same machine and the same headers.
const (
	ProductEMLy    = "emly"
	ProductUpdater = "updater"
)

// updaterUAPattern matches the EMLy Updater's User-Agent, e.g.
// "EMLy-Updater/1.3.0 (f.fois@3git.eu)".
var updaterUAPattern = regexp.MustCompile(`^EMLy-Updater/([\w.\-]+)\s*\(([^)]*)\)`)

func ParseUserAgent(ua string) (version, contact string) {
	m := updaterUAPattern.FindStringSubmatch(ua)
	if m == nil {
		return "", ""
	}
	return m[1], strings.TrimSpace(m[2])
}

func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Identity is everything one request tells us about the machine behind
// it: the X-EMLy-* headers the EMLy Updater sends, plus what the User-Agent
// and the connection add. Both telemetry paths (RecordEvent and
// trackConfigFetch) read the exact same set, so they read it here - a header
// wired into one and forgotten in the other is how a client ends up with a
// field that only updates on config fetches.
type Identity struct {
	HWID       string
	Hostname   string
	ADDomain   string
	LoggedUser string
	// LoggedUserState is one of the loggedUserState* values, or "" when the
	// header is absent or carries a value this server does not know.
	// LoggedUserDisconnectedAt is set only alongside loggedUserDisconnected.
	LoggedUserState          string
	LoggedUserDisconnectedAt time.Time
	// NobodyLoggedOn is true when the request positively says the machine
	// has no interactive user, as opposed to merely not saying who it is. See
	// reportsNobodyLoggedOn.
	NobodyLoggedOn bool
	Serial         string
	Product        string
	// OSVersion is the Windows release the machine runs, one opaque string
	// the updater renders from the registry ("Windows 11 24H2 Professional
	// (Build 26100.4652)"). Stored as sent; nothing parses it.
	OSVersion string
	// EMLyVersion is the version of the EMLy app (X-EMLy-AppVersion),
	// UAVersion the version of the updater that reported it (User-Agent).
	// They move independently: the updater self-updates on its own schedule,
	// the app only when a release is rolled out to this machine.
	EMLyVersion string
	UAVersion   string
	Contact     string
	IP          string
}

// identified reports whether the request carries enough to key a client row
// on. Everything else is optional detail.
func (c Identity) Identified() bool { return c.HWID != "" || c.Hostname != "" }

func IdentityFromRequest(r *http.Request) Identity {
	uaVersion, contact := ParseUserAgent(r.UserAgent())
	state, disconnectedAt := parseLoggedUserSession(
		r.Header.Get("X-EMLy-LoggedUserState"), r.Header.Get("X-EMLy-LoggedUserDisconnectedAt"))
	loggedUser := dbvalue.Truncate(r.Header.Get("X-EMLy-LoggedUser"), 255)
	return Identity{
		HWID:                     r.Header.Get("X-EMLy-HWID"),
		Hostname:                 r.Header.Get("X-EMLy-Hostname"),
		ADDomain:                 r.Header.Get("X-EMLy-ADDomain"),
		LoggedUser:               loggedUser,
		LoggedUserState:          state,
		LoggedUserDisconnectedAt: disconnectedAt,
		NobodyLoggedOn:           reportsNobodyLoggedOn(loggedUser, uaVersion),
		Serial:                   dbvalue.Truncate(r.Header.Get("X-EMLy-Serial"), 128),
		Product:                  dbvalue.Truncate(r.Header.Get("X-EMLy-Product"), 128),
		OSVersion:                dbvalue.Truncate(r.Header.Get("X-EMLy-OSVersion"), 128),
		EMLyVersion:              dbvalue.Truncate(r.Header.Get("X-EMLy-AppVersion"), 20),
		UAVersion:                uaVersion,
		Contact:                  contact,
		IP:                       ClientIP(r),
	}
}

// WSIdentityPayload is the "identity" message's data (design doc
// §3.1): the same set of facts the EMLy Updater already sends as X-EMLy-*
// headers on every manifest/download request, carried in JSON here instead.
// Every field is optional exactly like its header counterpart - absent means
// "not reported", not "empty" (see IdentityFromWSPayload).
type WSIdentityPayload struct {
	HWID                     string `json:"hwid"`
	Hostname                 string `json:"hostname"`
	ADDomain                 string `json:"ad_domain"`
	LoggedUser               string `json:"logged_user"`
	LoggedUserState          string `json:"logged_user_state"`
	LoggedUserDisconnectedAt string `json:"logged_user_disconnected_at"`
	Serial                   string `json:"serial"`
	Product                  string `json:"product"`
	OSVersion                string `json:"os_version"`
	EMLyVersion              string `json:"emly_version"`
}

// IdentityFromWSPayload builds a Identity from an "identity"
// message, the WS counterpart of IdentityFromRequest
// above: the same conversion rules
// (parseLoggedUserSession, reportsNobodyLoggedOn, truncate), just sourced
// from JSON fields instead of X-EMLy-* headers. updater_version/contact
// still come from the upgrade request's User-Agent, and ip from the
// connection - the identity message never carries either.
func IdentityFromWSPayload(r *http.Request, p WSIdentityPayload) Identity {
	uaVersion, contact := ParseUserAgent(r.UserAgent())
	state, disconnectedAt := parseLoggedUserSession(p.LoggedUserState, p.LoggedUserDisconnectedAt)
	loggedUser := dbvalue.Truncate(p.LoggedUser, 255)
	return Identity{
		HWID:                     p.HWID,
		Hostname:                 p.Hostname,
		ADDomain:                 p.ADDomain,
		LoggedUser:               loggedUser,
		LoggedUserState:          state,
		LoggedUserDisconnectedAt: disconnectedAt,
		NobodyLoggedOn:           reportsNobodyLoggedOn(loggedUser, uaVersion),
		Serial:                   dbvalue.Truncate(p.Serial, 128),
		Product:                  dbvalue.Truncate(p.Product, 128),
		OSVersion:                dbvalue.Truncate(p.OSVersion, 128),
		EMLyVersion:              dbvalue.Truncate(p.EMLyVersion, 20),
		UAVersion:                uaVersion,
		Contact:                  contact,
		IP:                       ClientIP(r),
	}
}

// Values of X-EMLy-LoggedUserState, and of updater_clients.logged_user_state.
// They are a wire contract with the EMLy Updater (machineinfo.SessionState).
const (
	loggedUserActiveConsole = "active-console"
	loggedUserActiveRDP     = "active-rdp"
	loggedUserDisconnected  = "disconnected"
)

// loggedUserSessionMinUpdaterVersion is the first EMLy Updater build that
// sends X-EMLy-LoggedUserState. From this version on the updater resolves the
// logged-on user on every request and sends the user and its state together,
// omitting both only when nobody is logged on.
const loggedUserSessionMinUpdaterVersion = "1.6.2"

// reportsNobodyLoggedOn tells "nobody is logged on" apart from "this client
// does not say". An updater older than loggedUserSessionMinUpdaterVersion
// sends no header in both cases, so its silence keeps the stored user. A
// newer one only omits X-EMLy-LoggedUser when the machine has no interactive
// session, so its silence is an answer, and keeping the previous user would
// show someone who signed out days ago as still logged on. A User-Agent that
// is not the updater's, or a version that does not parse, is not an answer.
func reportsNobodyLoggedOn(loggedUser, uaVersion string) bool {
	if loggedUser != "" || uaVersion == "" {
		return false
	}
	cmp, ok := compareDottedVersions(uaVersion, loggedUserSessionMinUpdaterVersion)
	return ok && cmp >= 0
}

// compareDottedVersions compares the numeric major.minor.patch prefix of two
// versions, ignoring any pre-release or build suffix ("1.6.2-dev" is 1.6.2).
// ok is false when either side has no numeric prefix to compare.
func compareDottedVersions(a, b string) (cmp int, ok bool) {
	pa, okA := dottedVersionParts(a)
	pb, okB := dottedVersionParts(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range pa {
		switch {
		case pa[i] < pb[i]:
			return -1, true
		case pa[i] > pb[i]:
			return 1, true
		}
	}
	return 0, true
}

func dottedVersionParts(v string) ([3]int, bool) {
	var parts [3]int
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	fields := strings.Split(v, ".")
	if len(fields) == 0 || len(fields) > 4 {
		return parts, false
	}
	for i, f := range fields {
		if i >= len(parts) {
			break
		}
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return parts, false
		}
		parts[i] = n
	}
	return parts, true
}

// parseLoggedUserSession validates the two session headers together.
//
// A state this server does not know comes back as "" - stored as "not
// reported" rather than as whatever string a client sent, so the column only
// ever holds values the dashboard can interpret. The disconnection time is
// only meaningful for a disconnected session and is dropped for any other
// state, as is one that does not parse as RFC 3339; it is normalised to UTC
// because that is how the DSN (loc=UTC) reads every DATETIME back.
func parseLoggedUserSession(state, disconnectedAt string) (string, time.Time) {
	state = strings.ToLower(strings.TrimSpace(state))
	switch state {
	case loggedUserActiveConsole, loggedUserActiveRDP:
		return state, time.Time{}
	case loggedUserDisconnected:
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(disconnectedAt))
		if err != nil {
			return state, time.Time{}
		}
		return state, t.UTC().Truncate(time.Second)
	default:
		return "", time.Time{}
	}
}

// RecordEvent best-effort persists an EMLy Updater client sighting and
// operation event. It never fails the caller's HTTP response - a client
// missing every identifying header is silently skipped, and DB errors are
// only logged, since this is telemetry on a client-facing path.
//
// Clients are identified by the X-EMLy-HWID header when present - it survives
// hostname renames/AD domain changes better than the old (hostname, ad_domain)
// pair. Clients that don't send it yet (not upgraded, or HWID unavailable on
// the machine) fall back to that old hostname-based identification, so both
// coexist during the rollout; a legacy row picks up its hwid the first time
// that same hostname/ad_domain shows up with the header set.
//
// product is ProductEMLy for traffic about the EMLy app and ProductUpdater for
// the updater's own self-update, so the two never get mixed in fleet stats.
//
// hub may be nil (tests, or a caller that doesn't care about the stats WS
// stream); when non-nil, a successfully recorded event is also published for
// GET /v2/stats/stream (docs/superpowers/specs/2026-09-04-websocket-stats-stream-design.md
// §6.1). The extra client-row fetch that publish needs only runs when
// hub.Active() - a quiet server with no dashboard connected pays nothing.
func RecordEvent(ctx context.Context, db *sqlx.DB, r *http.Request, hub *statshub.Hub, eventType, product, version string) {
	id := IdentityFromRequest(r)
	if !id.Identified() {
		return
	}

	if id.HWID == "" {
		// Legacy fallback: no HWID header, identify by hostname + ad_domain
		// as before.
		slog.WarnContext(ctx, "updater stats: missing X-EMLy-HWID header; using legacy hostname/ad_domain identification", "ip", dbvalue.NullString(id.IP), "hostname", dbvalue.NullString(id.Hostname), "ad_domain", dbvalue.NullString(id.ADDomain))
	}

	clientID, err := Upsert(ctx, db, id)
	if err != nil {
		slog.WarnContext(ctx, "updater stats: failed to upsert client", "error", err)
		return
	}

	eventVersion := dbvalue.NullString(version)
	eventIP := dbvalue.NullString(id.IP)
	eventID, err := insertEvent(ctx, db, clientID, eventType, product, eventVersion, eventIP)
	if err != nil {
		slog.WarnContext(ctx, "updater stats: failed to record event", "error", err)
		return
	}

	if hub == nil || !hub.Active() {
		return
	}
	var client models.UpdaterClient
	if err := db.GetContext(ctx, &client, `SELECT * FROM updater_clients WHERE id = ?`, clientID); err != nil {
		slog.WarnContext(ctx, "updater stats: failed to fetch client for stream publish", "error", err)
		return
	}
	hub.Publish(statshub.Event{
		Kind:   statshub.EventKindUpdaterEvent,
		Client: &client,
		EventEntry: &models.UpdaterEvent{
			ID:        eventID,
			ClientID:  int(clientID),
			EventType: eventType,
			Product:   product,
			Version:   eventVersion,
			IPAddress: eventIP,
			CreatedAt: time.Now().UTC(),
		},
	})
}

// insertEvent writes one updater_events row and, in the same
// transaction, adds it to its hour's updater_event_hourly counter (migration
// 20). It returns the new row's id.
//
// The two go together or not at all: the rollup is what /v2/stats/summary and
// /v2/stats/events actually read, so a raw insert that committed without its
// increment would be an event the dashboard never shows, and no later pass
// would notice. That is also why the counter is maintained here rather than by
// a periodic aggregation job - the number a dashboard reads is exactly as live
// as the event itself, which is what keeps /v2/stats/stream real-time now that
// it no longer touches raw rows.
//
// The bucket is derived from the inserted row's own created_at rather than a
// second NOW(), so every event lands in the bucket a GROUP BY over the raw
// rows would put it in - no row slipping into the next hour because the two
// statements straddled the boundary. That is what makes migration 20's
// backfill statement usable as a repair.
//
// Deletes are the other direction and deliberately do not follow: pruning
// (internal/eventprune) and DeleteStatsClient both remove raw rows without
// touching these counters, so the rollup is the fleet's aggregate history and
// keeps events the raw table no longer has. A repair can therefore only
// rebuild the retained window - which is the trade that lets the charts keep
// full history while the raw table stays bounded.
//
// Cost on this hot path is one extra primary-key lookup and one upsert against
// a handful of rows - the current hour's (product, event_type) pairs - which
// is why the read side can stop scanning half a million.
func insertEvent(ctx context.Context, db *sqlx.DB, clientID int64, eventType, product string, version, ip *string) (int64, error) {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit has run

	res, err := tx.ExecContext(ctx,
		`INSERT INTO updater_events (client_id, event_type, product, version, ip_address) VALUES (?, ?, ?, ?, ?)`,
		clientID, eventType, product, version, ip,
	)
	if err != nil {
		return 0, err
	}
	eventID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO updater_event_hourly (bucket_hour, product, event_type, count)
		 SELECT DATE_FORMAT(created_at, `+HourBucketExpr+`), product, event_type, 1
		   FROM updater_events WHERE id = ?
		 ON DUPLICATE KEY UPDATE count = count + 1`,
		eventID,
	); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return eventID, nil
}

// Upsert records one client sighting and returns its row id.
//
// updater_clients carries two independent unique keys - uniq_hwid and the
// legacy uniq_client (hostname, ad_domain) - so a blind
// INSERT/UPDATE ... ON DUPLICATE KEY UPDATE is unsafe here: whenever a
// single statement writes both a hwid and a (hostname, ad_domain) pair,
// either one can belong to a *different*, already-existing row (a hostname
// reused after reimage with a rotated hwid; a machine that reports under an
// incomplete header set once and the full set the next time; hand-testing
// with mismatched headers). MySQL then fails the whole statement with a
// duplicate-key error instead of merging anything.
//
// This resolves it in three explicit steps instead of one implicit one:
//  1. find the target row - by hwid if given, else by (hostname, ad_domain);
//  2. delete any *other* row that currently squats on the (hostname,
//     ad_domain) pair we're about to write into the target (or into a fresh
//     row) - hwid is the authoritative identity once present, so a stale row
//     merely holding that hostname is superseded and its (best-effort,
//     non-authoritative) history can be dropped along with it;
//  3. update the target row by id, or insert a fresh one.
//
// Every write after step 2 targets a single row with no remaining unique
// value collision possible.
//
// A header the request does not carry leaves the stored value alone rather
// than clearing it (the COALESCE/NULLIF pairs below). "Not reported" and
// "reported as empty" are the same thing on the wire - the updater omits a
// header it has no value for - and for logged_user/serial/product the last
// known answer is worth more than a NULL: an updater too old to send them
// must not erase what earlier sightings established. os_version behaves the
// same way: a machine that stops reporting it has not gone back to an unknown
// Windows build. last_seen_at is what says how current the row is.
//
// The logged-user columns have two exceptions:
//   - when the request positively reports that nobody is logged on
//     (id.NobodyLoggedOn - an updater new enough that its silence is an
//     answer), logged_user, logged_user_state and logged_user_disconnected_at
//     are all cleared, otherwise a user who signed out stays on the row for
//     as long as the machine keeps checking in;
//   - logged_user_disconnected_at follows logged_user_state instead of its
//     own header. Whenever a state is reported the timestamp is written with
//     it - NULL included - because a session that reconnects stops sending
//     the header, and keeping the old value would leave a row saying
//     "active-rdp, disconnected since yesterday".
func Upsert(ctx context.Context, db *sqlx.DB, id Identity) (int64, error) {
	hwid, hostname, adDomain := id.HWID, id.Hostname, id.ADDomain
	lookup := func(query string, args ...interface{}) (int64, bool, error) {
		var id int64
		err := db.GetContext(ctx, &id, query, args...)
		switch {
		case err == nil:
			return id, true, nil
		case errors.Is(err, sql.ErrNoRows):
			return 0, false, nil
		default:
			return 0, false, err
		}
	}

	var targetID int64
	var haveTarget bool
	var err error

	if hwid != "" {
		targetID, haveTarget, err = lookup(`SELECT id FROM updater_clients WHERE hwid = ?`, hwid)
		if err != nil {
			return 0, err
		}
	}
	if !haveTarget && hostname != "" {
		targetID, haveTarget, err = lookup(
			`SELECT id FROM updater_clients WHERE hostname = ? AND ad_domain = ?`, hostname, adDomain)
		if err != nil {
			return 0, err
		}
	}

	if hostname != "" {
		deleteQuery := `DELETE FROM updater_clients WHERE hostname = ? AND ad_domain = ?`
		deleteArgs := []interface{}{hostname, adDomain}
		if haveTarget {
			deleteQuery += ` AND id != ?`
			deleteArgs = append(deleteArgs, targetID)
		}
		if _, err := db.ExecContext(ctx, deleteQuery, deleteArgs...); err != nil {
			return 0, err
		}
	}

	if haveTarget {
		if _, err := db.ExecContext(ctx,
			`UPDATE updater_clients
			 SET hwid = COALESCE(NULLIF(?, ''), hwid), hostname = ?, ad_domain = ?,
			     logged_user = IF(?, NULL, COALESCE(NULLIF(?, ''), logged_user)),
			     logged_user_disconnected_at = IF(? OR ? <> '', ?, logged_user_disconnected_at),
			     logged_user_state = IF(?, NULL, COALESCE(NULLIF(?, ''), logged_user_state)),
			     serial = COALESCE(NULLIF(?, ''), serial),
			     product = COALESCE(NULLIF(?, ''), product),
			     os_version = COALESCE(NULLIF(?, ''), os_version),
			     emly_version = COALESCE(NULLIF(?, ''), emly_version),
			     updater_version = ?, contact = ?, last_ip = ?, last_seen_at = CURRENT_TIMESTAMP
			 WHERE id = ?`,
			hwid, hostname, adDomain,
			id.NobodyLoggedOn, id.LoggedUser,
			id.NobodyLoggedOn, id.LoggedUserState, dbvalue.NullTime(id.LoggedUserDisconnectedAt),
			id.NobodyLoggedOn, id.LoggedUserState,
			id.Serial, id.Product, id.OSVersion, id.EMLyVersion,
			dbvalue.NullString(id.UAVersion), dbvalue.NullString(id.Contact), dbvalue.NullString(id.IP), targetID,
		); err != nil {
			return 0, err
		}
		return targetID, nil
	}

	res, err := db.ExecContext(ctx,
		`INSERT INTO updater_clients (hwid, hostname, ad_domain, logged_user, logged_user_state, logged_user_disconnected_at, serial, product, os_version, emly_version, updater_version, contact, last_ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		dbvalue.NullString(hwid), hostname, adDomain,
		dbvalue.NullString(id.LoggedUser), dbvalue.NullString(id.LoggedUserState), dbvalue.NullTime(id.LoggedUserDisconnectedAt),
		dbvalue.NullString(id.Serial), dbvalue.NullString(id.Product),
		dbvalue.NullString(id.OSVersion), dbvalue.NullString(id.EMLyVersion),
		dbvalue.NullString(id.UAVersion), dbvalue.NullString(id.Contact), dbvalue.NullString(id.IP),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}