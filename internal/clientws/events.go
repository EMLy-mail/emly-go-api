package clientws

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/clienthub"
	"emly-api-go/internal/clientproto"
	"emly-api-go/internal/dbvalue"
	"emly-api-go/internal/presencehub"
	"emly-api-go/internal/updaterclient"
)

// updateLoggedUserFn is the seam tests replace to observe session.changed
// without a database, like upsertClientFn.
var updateLoggedUserFn = updaterclient.UpdateLoggedUser

// updateLastStopFn is the same seam for service.stopped / last_stop.
var updateLastStopFn = updaterclient.UpdateLastStop

// handleEvent records an event and then applies its side effects. Unknown
// names are recorded too: the dashboard can show what it cannot interpret.
// The record is written before the side effect runs (not after) so that a
// caller synchronized on the side effect - updateLoggedUserFn, in
// particular, which a test observes through a channel - can rely on
// hub.Events already reflecting this event by the time it wakes up: the
// write happens-before the send that wakes it (Go's channel memory model),
// only when the write precedes the send in program order.
//
// presence and tok are this connection's presence registration: a
// service.stopped marks it, so the client reads as offline for that reason
// once it drops.
func handleEvent(ctx context.Context, db *sqlx.DB, hub *clienthub.Hub, presence *presencehub.Hub, tok presencehub.Token, clientID int64, uaVersion string, env clientproto.Envelope, ev clientproto.Event) {
	slog.InfoContext(ctx, "client ws: event", "client_id", clientID, "name", ev.Name)
	hub.RecordEvent(clienthub.EventRecord{
		ID: env.ID, ClientID: clientID, Name: ev.Name, ReceivedAt: timeNow(),
		ClientTS: env.TS, Payload: ev.Payload,
	})

	switch ev.Name {
	case clientproto.EvtSessionChanged:
		var p clientproto.SessionChanged
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			slog.DebugContext(ctx, "client ws: bad session.changed payload", "client_id", clientID, "error", err)
			return
		}
		if p.Changed {
			var u clientproto.UserSession
			if p.LoggedUser != nil {
				u = *p.LoggedUser
			}
			who := updaterclient.LoggedUserFromWS(uaVersion, u.User, u.State, u.DisconnectedAt)
			if err := updateLoggedUserFn(ctx, db, clientID, who); err != nil {
				slog.WarnContext(ctx, "client ws: failed to update logged user", "client_id", clientID, "error", err)
			}
		}
	case clientproto.EvtServiceStarted:
		var p clientproto.ServiceStarted
		if err := json.Unmarshal(ev.Payload, &p); err == nil {
			hub.CompleteByRestart(clientID, p.CompletedCommands)
			if p.LastStop != nil {
				slog.InfoContext(ctx, "client ws: agent restarted", "client_id", clientID,
					"reason", p.Reason, "last_stop", p.LastStop.Reason, "last_stop_sent", p.LastStop.Sent)
				// Written whether or not sent: with sent=true it rewrites
				// what service.stopped already stored, which is harmless,
				// and covers a live write that failed.
				if stop, ok := stopFromWire(p.LastStop.Reason, p.LastStop.Category, p.LastStop.At); ok {
					recordLastStop(ctx, db, clientID, stop)
				}
			}
		}
	case clientproto.EvtServiceStopped:
		// The connection drops right after. Presence keeps the reason for
		// that offline spell, the client row keeps it past the event ring.
		var p clientproto.ServiceStopped
		if err := json.Unmarshal(ev.Payload, &p); err == nil {
			slog.InfoContext(ctx, "client ws: agent stopping", "client_id", clientID,
				"reason", p.Reason, "category", p.Category, "command_id", p.CommandID)
			if stop, ok := stopFromWire(p.Reason, p.Category, p.At); ok {
				presence.Stopping(tok, stop)
				recordLastStop(ctx, db, clientID, stop)
			}
		}
	}
}

// stopFromWire turns the reason/category/at of a service.stopped or a
// last_stop into what is stored. Values the server does not know are kept
// as they are (spec: the list may grow), only capped to the column width.
// A missing category is derived from the reason; an absent or malformed at
// is zero (NULL), as it is by design for "unexpected". No reason, nothing
// to record.
func stopFromWire(reason, category, at string) (presencehub.Stop, bool) {
	reason = dbvalue.Truncate(reason, 20)
	if reason == "" {
		return presencehub.Stop{}, false
	}
	if category == "" {
		category = clientproto.StopCategory(reason)
	}
	s := presencehub.Stop{Reason: reason, Category: dbvalue.Truncate(category, 20)}
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		s.At = t.UTC()
	}
	return s, true
}

func recordLastStop(ctx context.Context, db *sqlx.DB, clientID int64, s presencehub.Stop) {
	if err := updateLastStopFn(ctx, db, clientID, s.Reason, s.Category, s.At); err != nil {
		slog.WarnContext(ctx, "client ws: failed to update last stop", "client_id", clientID, "error", err)
	}
}
