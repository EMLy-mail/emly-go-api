package clientws

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/presencehub"
	"emly-api-go/internal/updaterclient"
)

// clientWSInMessage is the client->server envelope for GET /v2/client/ws:
// only "identity" and "pong" are meaningful today (design doc §3), but the
// shape stays generic so a future message type doesn't need a new envelope.
type clientWSInMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

const (
	// clientWSHelloTimeout bounds how long the server waits for the
	// client's first message (design doc §3.1: "entro 10s").
	clientWSHelloTimeout = 10 * time.Second
	// clientWSPingInterval/clientWSIdleTimeout implement the heartbeat
	// (design doc §3.2): a ping every 10s, and the connection is torn down
	// if nothing at all arrives within 20s (two intervals).
	clientWSPingInterval = 10 * time.Second
	clientWSIdleTimeout  = 20 * time.Second
	// clientWSWriteTimeout bounds one outbound frame, so a client that has
	// stopped reading cannot wedge the handler.
	clientWSWriteTimeout = 10 * time.Second
)

// clientWSEnvelope is the server->client message envelope for this route.
// It is deliberately not the stats stream's envelope, which it borrowed until
// these became separate packages: that one carries a channel and a timestamp
// because a subscriber multiplexes several channels over one socket, and
// neither field was ever set here. Two protocols, two envelopes - the bytes on
// the wire are unchanged, since the unused fields were omitempty.
//
// The type field is open-ended on purpose: today only hello/identity/ping/pong
// exist, and an unrecognized type is logged and ignored rather than closing the
// connection, so a future message type can roll out without breaking an
// already-deployed updater.
type clientWSEnvelope struct {
	Type string      `json:"type"`
	Data interface{} `json:"data,omitempty"`
}

// upsertClientFn is the seam ClientWS calls through instead of
// updaterclient.Upsert directly. Production behavior is unchanged (it
// defaults to the real function); a test replaces it to stub the DB-touching
// upsert and drive the handshake -> presence-online path end to end without
// a live database (see TestClientWSIdentityMarksClientOnline).
var upsertClientFn = updaterclient.Upsert

func writeClientWSEnvelope(ctx context.Context, c *websocket.Conn, typ string, data interface{}) error {
	env := clientWSEnvelope{Type: typ, Data: data}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, clientWSWriteTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}

func writeClientWSError(ctx context.Context, c *websocket.Conn, code, message string) {
	_ = writeClientWSEnvelope(ctx, c, "error", map[string]string{"code": code, "message": message})
}

// readClientIdentity reads exactly one message and requires it to be a
// well-formed, identified "identity" message (design doc §3.1). Any other
// outcome sends an `error` envelope and returns ok=false; the caller closes
// the connection.
func readClientIdentity(ctx context.Context, c *websocket.Conn, r *http.Request) (updaterclient.Identity, bool) {
	rctx, cancel := context.WithTimeout(ctx, clientWSHelloTimeout)
	defer cancel()

	_, data, err := c.Read(rctx)
	if err != nil {
		return updaterclient.Identity{}, false
	}

	var msg clientWSInMessage
	if err := json.Unmarshal(data, &msg); err != nil || msg.Type != "identity" {
		writeClientWSError(ctx, c, "unidentified", "expected an identity message")
		return updaterclient.Identity{}, false
	}

	var payload updaterclient.WSIdentityPayload
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		writeClientWSError(ctx, c, "invalid_params", "invalid identity payload")
		return updaterclient.Identity{}, false
	}

	id := updaterclient.IdentityFromWSPayload(r, payload)
	if !id.Identified() {
		writeClientWSError(ctx, c, "unidentified", "identity carries neither hwid nor hostname")
		return updaterclient.Identity{}, false
	}
	return id, true
}

// clientWSReadLoop is the connection's single reader. Each Read is bounded
// by clientWSIdleTimeout, so the loop exits the moment 20s pass without a
// single frame from the client, "pong" included (design doc §3.2). An
// unrecognized message type is logged and ignored, never a reason to close
// (design doc §3.3): that is what lets a future message type roll out
// without breaking already-deployed updaters.
func clientWSReadLoop(ctx context.Context, c *websocket.Conn) {
	for {
		rctx, cancel := context.WithTimeout(ctx, clientWSIdleTimeout)
		_, data, err := c.Read(rctx)
		cancel()
		if err != nil {
			return
		}

		var msg clientWSInMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "pong":
			// Keepalive only - reading anything at all already reset the
			// idle timer above.
		default:
			slog.DebugContext(ctx, "client ws: unrecognized message type", "type", msg.Type)
		}
	}
}

// clientWSPingLoop sends the heartbeat (design doc §3.2). It is the only
// writer once the handshake completes, so no write mutex is needed. It
// exits on ctx cancellation or the first failed write.
func clientWSPingLoop(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(clientWSPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := writeClientWSEnvelope(ctx, c, "ping", nil); err != nil {
				return
			}
		}
	}
}

// ClientWS handles GET /v2/client/ws (design doc §2/§3). Auth
// (apimw.APIKeyAuth) runs before this handler as route middleware, same as
// the updater's self-update manifest - unlike /v2/stats/stream, there is no
// query-string key fallback to justify an inline check here. presence may be
// nil (tests, or a build that never constructs one); presencehub.Hub's own
// methods (including Connect/Disconnect) all tolerate a nil receiver, so a
// nil presence simply never tracks anyone as online.
func ClientWS(db *sqlx.DB, presence *presencehub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			slog.WarnContext(r.Context(), "client ws: upgrade failed", "error", err)
			return
		}

		ctx, cancel := context.WithCancel(r.Context())

		if err := writeClientWSEnvelope(ctx, c, "hello", nil); err != nil {
			cancel()
			c.Close(websocket.StatusInternalError, "")
			return
		}

		identity, ok := readClientIdentity(ctx, c, r)
		if !ok {
			cancel()
			c.Close(websocket.StatusPolicyViolation, "identity required")
			return
		}

		clientID, err := upsertClientFn(ctx, db, identity)
		if err != nil {
			slog.WarnContext(ctx, "client ws: failed to upsert client", "error", err)
			cancel()
			c.Close(websocket.StatusInternalError, "")
			return
		}

		tok, supersede := presence.Connect(clientID)
		slog.InfoContext(ctx, "client ws: connection established", "client_id", clientID, "hostname", identity.Hostname)

		var superseded bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); clientWSPingLoop(ctx, c) }()
		go func() {
			defer wg.Done()
			select {
			case <-supersede:
				superseded = true
				cancel()
			case <-ctx.Done():
			}
		}()

		clientWSReadLoop(ctx, c)

		cancel()
		wg.Wait()
		presence.Disconnect(tok)
		if superseded {
			// design doc §5: a superseded connection is told why, not just
			// dropped with the generic "normal closure" every other teardown
			// path (idle timeout, client disconnect, server shutdown) uses.
			c.Close(websocket.StatusPolicyViolation, "superseded by newer connection")
			return
		}
		c.Close(websocket.StatusNormalClosure, "")
	}
}
