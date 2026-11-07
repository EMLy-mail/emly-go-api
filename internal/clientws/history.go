package clientws

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/clientproto"
	"emly-api-go/internal/dbvalue"
)

// This file owns updater_client_ws_events (migration 30): the durable
// history of the events machines send on this channel, beyond the 50-entry
// in-memory ring of internal/clienthub. internal/eventprune trims (and with
// EVENTS_ARCHIVE_ENABLED archives) it after CLIENT_WS_EVENTS_RETENTION_DAYS.

const (
	// maxStoredPayload caps the JSON kept per row. A frame is already capped
	// at clientproto.DefaultLimits.MaxMessageBytes (64 KiB); every event of
	// §8 is well under 1 KiB except machine.info, which is not stored. A
	// payload over the cap is a client bug or abuse, so the row is kept -
	// that the event happened is still worth knowing - without it.
	maxStoredPayload = 16 << 10

	// storeTimeout bounds the insert, which runs on the connection's read
	// loop: a slow database must not stall the reads that answer pings.
	storeTimeout = 5 * time.Second
)

// storedEvents are the event names written to the table. Known names only:
// an unknown one still lands in the hub's ring (the dashboard shows it), but
// a client inventing names must not be able to grow a durable table with
// them. machine.info is absent on purpose: it is state, already folded into
// updater_clients by Upsert, and at 1-2 KiB a row it would be most of the
// table's size while telling nothing a history needs.
var storedEvents = map[string]bool{
	clientproto.EvtSessionChanged:  true,
	clientproto.EvtUpdateAvailable: true,
	clientproto.EvtUpdateStarted:   true,
	clientproto.EvtUpdateApplied:   true,
	clientproto.EvtUpdateFailed:    true,
	clientproto.EvtServiceStarted:  true,
	clientproto.EvtServiceStopped:  true,
	clientproto.EvtMachinePower:    true,
}

// wsEventRow is one row of updater_client_ws_events.
type wsEventRow struct {
	EventID        string
	ClientID       int64
	Name           string
	Payload        []byte // nil = SQL NULL
	PayloadDropped bool
	ClientTS       *time.Time
}

// insertWSEventFn is the seam tests replace to observe what is stored
// without a database, like updateLoggedUserFn.
var insertWSEventFn = insertWSEvent

// shouldStore says whether an event goes to the table. session.changed is
// stored only when the logged-on user actually changed: lock/unlock of the
// same user is most of that event's traffic and none of its history.
func shouldStore(name string, payload json.RawMessage) bool {
	if !storedEvents[name] {
		return false
	}
	if name == clientproto.EvtSessionChanged {
		var p struct {
			Changed bool `json:"changed"`
		}
		if err := json.Unmarshal(payload, &p); err != nil || !p.Changed {
			return false
		}
	}
	return true
}

// buildWSEventRow turns a received event into its row. The envelope id is
// the dedup key (a re-sent buffered event is stored once); one that is not a
// ULID's length gets a server-minted id instead, so a malformed client can
// neither collide with other rows nor overflow the column.
func buildWSEventRow(clientID int64, env clientproto.Envelope, ev clientproto.Event) wsEventRow {
	row := wsEventRow{EventID: env.ID, ClientID: clientID, Name: dbvalue.Truncate(ev.Name, 40)}
	if len(row.EventID) != 26 {
		row.EventID = clientproto.NewID()
	}
	switch {
	case len(ev.Payload) > maxStoredPayload:
		row.PayloadDropped = true
	case len(ev.Payload) > 0 && json.Valid(ev.Payload):
		row.Payload = ev.Payload
	}
	if t, err := time.Parse(time.RFC3339, env.TS); err == nil {
		t = t.UTC()
		row.ClientTS = &t
	}
	return row
}

// storeWSEvent writes the event to updater_client_ws_events when it is one
// worth keeping. A failure is logged and otherwise ignored: the event is
// already in the hub's ring, and losing a history row must never cost the
// connection.
func storeWSEvent(ctx context.Context, db *sqlx.DB, clientID int64, env clientproto.Envelope, ev clientproto.Event) {
	if !shouldStore(ev.Name, ev.Payload) {
		return
	}
	row := buildWSEventRow(clientID, env, ev)
	if row.PayloadDropped {
		slog.WarnContext(ctx, "client ws: event payload too large to store, kept without it",
			"client_id", clientID, "name", ev.Name, "bytes", len(ev.Payload))
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	if err := insertWSEventFn(ctx, db, row); err != nil {
		slog.WarnContext(ctx, "client ws: failed to store event", "client_id", clientID, "name", ev.Name, "error", err)
	}
}

func insertWSEvent(ctx context.Context, db *sqlx.DB, row wsEventRow) error {
	if db == nil {
		return nil
	}
	var payload any // a nil []byte must reach the driver as NULL, not ''
	if row.Payload != nil {
		payload = string(row.Payload)
	}
	// IGNORE: the expected conflict is uq_event_id, i.e. the same event
	// delivered twice, which is exactly the row already there. It also turns
	// a machine deleted while still connected (FK) into a skipped row rather
	// than an error per event.
	_, err := db.ExecContext(ctx,
		`INSERT IGNORE INTO updater_client_ws_events
		     (event_id, client_id, name, payload, payload_dropped, client_ts)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		row.EventID, row.ClientID, row.Name, payload, row.PayloadDropped, row.ClientTS)
	return err
}
