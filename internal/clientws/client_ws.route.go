package clientws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/clienthub"
	"emly-api-go/internal/clientproto"
	"emly-api-go/internal/presencehub"
	"emly-api-go/internal/updaterclient"
)

// clientWSInMessage is the client->server envelope for GET /v2/client/ws:
// only "identity" and "pong" are meaningful in v1 (design doc §3), but the
// shape stays generic so a future message type doesn't need a new envelope.
// v2 messages (ack/result/event) are parsed straight into clientproto.Envelope
// by clientWSReadLoop instead - this type only serves the pre-handshake read.
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

// clientWSEnvelope is the server->client message envelope used by this
// file's v1-era tests to read a frame's "type" generically. Production code
// writes frames with clientproto.Frame/writeFrame now; this type is kept
// only so those tests don't need to depend on clientproto.Envelope's exact
// shape to make the same assertion ("type" == "hello"/"error"/...).
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

// timeNow is the clock; a var so tests could pin it.
var timeNow = time.Now

func writeFrame(ctx context.Context, c *websocket.Conn, typ, replyTo string, data any) error {
	b, err := clientproto.Frame(typ, replyTo, data, timeNow())
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, clientWSWriteTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}

func writeClientWSError(ctx context.Context, c *websocket.Conn, code, message string) {
	_ = writeFrame(ctx, c, clientproto.TypeError, "", clientproto.ErrorBody{Code: code, Message: message})
}

// readClientIdentity reads exactly one message and requires it to be a
// well-formed, identified "identity" message (design doc §3.1). Any other
// outcome sends an `error` envelope and returns ok=false; the caller closes
// the connection. The returned IdentityExt carries v2's protocol/capabilities
// (zero value for a v1 client, which sends neither).
func readClientIdentity(ctx context.Context, c *websocket.Conn, r *http.Request) (updaterclient.Identity, clientproto.IdentityExt, bool) {
	rctx, cancel := context.WithTimeout(ctx, clientWSHelloTimeout)
	defer cancel()

	_, data, err := c.Read(rctx)
	if err != nil {
		return updaterclient.Identity{}, clientproto.IdentityExt{}, false
	}

	var msg clientWSInMessage
	if err := json.Unmarshal(data, &msg); err != nil || msg.Type != "identity" {
		writeClientWSError(ctx, c, "unidentified", "expected an identity message")
		return updaterclient.Identity{}, clientproto.IdentityExt{}, false
	}

	var payload updaterclient.WSIdentityPayload
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		writeClientWSError(ctx, c, "invalid_params", "invalid identity payload")
		return updaterclient.Identity{}, clientproto.IdentityExt{}, false
	}

	id := updaterclient.IdentityFromWSPayload(r, payload)
	if !id.Identified() {
		writeClientWSError(ctx, c, "unidentified", "identity carries neither hwid nor hostname")
		return updaterclient.Identity{}, clientproto.IdentityExt{}, false
	}

	var ext clientproto.IdentityExt
	_ = json.Unmarshal(msg.Data, &ext)

	return id, ext, true
}

// errRateLimited ends the read loop after the rate_limited error was sent.
var errRateLimited = errors.New("rate limited")

// clientWSReadLoop is the connection's single reader (see the v1 comment:
// the idle deadline and "unknown type is ignored" still hold). In v2 it also
// dispatches ack/result to the hub and events to handleEvent.
func clientWSReadLoop(ctx context.Context, c *websocket.Conn, db *sqlx.DB, hub *clienthub.Hub, clientID int64, uaVersion string, v2 bool) error {
	limiter := newEventLimiter(clientproto.DefaultLimits.MaxEventsPerMinute, time.Minute, timeNow)
	for {
		rctx, cancel := context.WithTimeout(ctx, clientWSIdleTimeout)
		_, data, err := c.Read(rctx)
		cancel()
		if err != nil {
			return err
		}

		var env clientproto.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		switch {
		case env.Type == clientproto.TypePong:
			// Keepalive only - reading anything at all already reset the
			// idle timer above.
		case v2 && env.Type == clientproto.TypeAck:
			var ack clientproto.Ack
			if json.Unmarshal(env.Data, &ack) == nil {
				hub.HandleAck(clientID, env.ReplyTo, ack)
			}
		case v2 && env.Type == clientproto.TypeResult:
			var res clientproto.Result
			if json.Unmarshal(env.Data, &res) == nil {
				hub.HandleResult(clientID, env.ReplyTo, res)
			}
		case v2 && env.Type == clientproto.TypeEvent:
			if !limiter.allow() {
				writeClientWSError(ctx, c, clientproto.ErrRateLimited, "too many events")
				return errRateLimited
			}
			var ev clientproto.Event
			if json.Unmarshal(env.Data, &ev) == nil {
				handleEvent(ctx, db, hub, clientID, uaVersion, env, ev)
			}
		default:
			slog.DebugContext(ctx, "client ws: unrecognized message type", "type", env.Type)
		}
	}
}

// eventLimiter is a fixed-window counter: at most max events per window.
type eventLimiter struct {
	max    int
	window time.Duration
	now    func() time.Time
	start  time.Time
	count  int
}

func newEventLimiter(max int, window time.Duration, now func() time.Time) *eventLimiter {
	return &eventLimiter{max: max, window: window, now: now, start: now()}
}

func (l *eventLimiter) allow() bool {
	if t := l.now(); t.Sub(l.start) >= l.window {
		l.start, l.count = t, 0
	}
	l.count++
	return l.count <= l.max
}

// clientWSPingLoop sends the heartbeat (design doc §3.2). It is the only
// writer once the handshake completes and before the hub is attached, so no
// write mutex is needed against it; once attached, the hub's Sender (this
// same c.Write) may run concurrently from a command goroutine, which
// coder/websocket's Conn permits (Read/Reader excepted). It exits on ctx
// cancellation or the first failed write.
func clientWSPingLoop(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(clientWSPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := writeFrame(ctx, c, clientproto.TypePing, "", nil); err != nil {
				return
			}
		}
	}
}

// ClientWS handles GET /v2/client/ws (design doc §2/§3, CLIENT_WS_PROTOCOL.md
// for v2). Auth (apimw.APIKeyAuth) runs before this handler as route
// middleware, same as the updater's self-update manifest - unlike
// /v2/stats/stream, there is no query-string key fallback to justify an
// inline check here. presence may be nil (tests, or a build that never
// constructs one); presencehub.Hub's own methods (including
// Connect/Disconnect) all tolerate a nil receiver, so a nil presence simply
// never tracks anyone as online. hub may be nil the same way (clienthub.Hub
// is nil-receiver safe throughout): a nil hub keeps v1 behavior exactly as
// before and a v2 client simply gets no commands/notifies.
func ClientWS(db *sqlx.DB, presence *presencehub.Hub, hub *clienthub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			slog.WarnContext(r.Context(), "client ws: upgrade failed", "error", err)
			return
		}
		// Enforced by the library itself: it closes with 1009
		// (StatusMessageTooBig) the moment any single message - handshake
		// included - exceeds this, so no manual check is needed anywhere
		// else in this handler.
		c.SetReadLimit(int64(clientproto.DefaultLimits.MaxMessageBytes))

		ctx, cancel := context.WithCancel(r.Context())

		if err := writeFrame(ctx, c, clientproto.TypeHello, "", clientproto.Hello{
			Protocol:   clientproto.ProtocolCurrent,
			ServerTime: timeNow().UTC().Format(time.RFC3339),
		}); err != nil {
			cancel()
			c.Close(websocket.StatusInternalError, "")
			return
		}

		identity, ext, ok := readClientIdentity(ctx, c, r)
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

		sender := func(sctx context.Context, frame []byte) error {
			return c.Write(sctx, websocket.MessageText, frame)
		}

		v2 := ext.Protocol >= clientproto.ProtocolV2
		var detach func()
		if v2 {
			accepted := clientproto.Intersect(ext.Capabilities, clientproto.ServerCapabilities)
			if err := writeFrame(ctx, c, clientproto.TypeWelcome, "", clientproto.Welcome{
				Protocol: clientproto.ProtocolV2, AcceptedCapabilities: accepted, Limits: clientproto.DefaultLimits,
			}); err != nil {
				// Same teardown as an upsert failure, plus the presence
				// token this path has already acquired.
				slog.WarnContext(ctx, "client ws: failed to send welcome", "client_id", clientID, "error", err)
				cancel()
				presence.Disconnect(tok)
				c.Close(websocket.StatusInternalError, "")
				return
			}
			detach = hub.Attach(clientID, clientproto.ProtocolV2, accepted, sender)
		} else {
			// A v1 client is attached too, with ProtocolV1 and no
			// capabilities, so Issue answers ErrUnsupported rather than
			// ErrOffline for it - the admin sees the real reason.
			detach = hub.Attach(clientID, clientproto.ProtocolV1, nil, sender)
		}
		defer detach()

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

		loopErr := clientWSReadLoop(ctx, c, db, hub, clientID, identity.UAVersion, v2)

		cancel()
		wg.Wait()
		presence.Disconnect(tok)
		if errors.Is(loopErr, errRateLimited) {
			c.Close(websocket.StatusPolicyViolation, "rate limited")
			return
		}
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
