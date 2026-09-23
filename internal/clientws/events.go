package clientws

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/clienthub"
	"emly-api-go/internal/clientproto"
	"emly-api-go/internal/updaterclient"
)

// updateLoggedUserFn is the seam tests replace to observe session.changed
// without a database, like upsertClientFn.
var updateLoggedUserFn = updaterclient.UpdateLoggedUser

// handleEvent records an event and then applies its side effects. Unknown
// names are recorded too: the dashboard can show what it cannot interpret.
// The record is written before the side effect runs (not after) so that a
// caller synchronized on the side effect - updateLoggedUserFn, in
// particular, which a test observes through a channel - can rely on
// hub.Events already reflecting this event by the time it wakes up: the
// write happens-before the send that wakes it (Go's channel memory model),
// only when the write precedes the send in program order.
func handleEvent(ctx context.Context, db *sqlx.DB, hub *clienthub.Hub, clientID int64, uaVersion string, env clientproto.Envelope, ev clientproto.Event) {
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
		}
	}
}
