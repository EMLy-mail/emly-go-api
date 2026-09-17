package handlers

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
)

// clientWSInMessage is the client->server envelope for GET /v2/client/ws:
// only "identity" and "pong" are meaningful today (design doc §3), but the
// shape stays generic so a future message type doesn't need a new envelope.
type clientWSInMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// clientWSIdentityPayload is the "identity" message's data (design doc
// §3.1): the same set of facts the EMLy Updater already sends as X-EMLy-*
// headers on every manifest/download request, carried in JSON here instead.
// Every field is optional exactly like its header counterpart - absent means
// "not reported", not "empty" (see clientIdentityFromWSPayload).
type clientWSIdentityPayload struct {
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

// clientIdentityFromWSPayload builds a clientIdentity from an "identity"
// message, the WS counterpart of clientIdentityFromRequest
// (internal/handlers/updates.route.go): the same conversion rules
// (parseLoggedUserSession, reportsNobodyLoggedOn, truncate), just sourced
// from JSON fields instead of X-EMLy-* headers. updater_version/contact
// still come from the upgrade request's User-Agent, and ip from the
// connection - the identity message never carries either.
func clientIdentityFromWSPayload(r *http.Request, p clientWSIdentityPayload) clientIdentity {
	uaVersion, contact := parseUpdaterUserAgent(r.UserAgent())
	state, disconnectedAt := parseLoggedUserSession(p.LoggedUserState, p.LoggedUserDisconnectedAt)
	loggedUser := truncate(p.LoggedUser, 255)
	return clientIdentity{
		HWID:                     p.HWID,
		Hostname:                 p.Hostname,
		ADDomain:                 p.ADDomain,
		LoggedUser:               loggedUser,
		LoggedUserState:          state,
		LoggedUserDisconnectedAt: disconnectedAt,
		NobodyLoggedOn:           reportsNobodyLoggedOn(loggedUser, uaVersion),
		Serial:                   truncate(p.Serial, 128),
		Product:                  truncate(p.Product, 128),
		OSVersion:                truncate(p.OSVersion, 128),
		EMLyVersion:              truncate(p.EMLyVersion, 20),
		UAVersion:                uaVersion,
		Contact:                  contact,
		IP:                       clientIPFromRequest(r),
	}
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
)

func writeClientWSEnvelope(ctx context.Context, c *websocket.Conn, typ string, data interface{}) error {
	env := wsEnvelopeOut{Type: typ, Data: data}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
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
func readClientIdentity(ctx context.Context, c *websocket.Conn, r *http.Request) (clientIdentity, bool) {
	rctx, cancel := context.WithTimeout(ctx, clientWSHelloTimeout)
	defer cancel()

	_, data, err := c.Read(rctx)
	if err != nil {
		return clientIdentity{}, false
	}

	var msg clientWSInMessage
	if err := json.Unmarshal(data, &msg); err != nil || msg.Type != "identity" {
		writeClientWSError(ctx, c, "unidentified", "expected an identity message")
		return clientIdentity{}, false
	}

	var payload clientWSIdentityPayload
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		writeClientWSError(ctx, c, "invalid_params", "invalid identity payload")
		return clientIdentity{}, false
	}

	id := clientIdentityFromWSPayload(r, payload)
	if !id.identified() {
		writeClientWSError(ctx, c, "unidentified", "identity carries neither hwid nor hostname")
		return clientIdentity{}, false
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
// nil (tests); presencehub.Hub's own methods tolerate that, but Connect on a
// nil Hub panics, so callers that pass nil must not expect presence
// tracking to do anything (only exercised by tests that never send a valid
// identity).
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

		clientID, err := upsertUpdaterClient(ctx, db, identity)
		if err != nil {
			slog.WarnContext(ctx, "client ws: failed to upsert client", "error", err)
			cancel()
			c.Close(websocket.StatusInternalError, "")
			return
		}

		tok, supersede := presence.Connect(clientID)
		slog.InfoContext(ctx, "client ws: connection established", "client_id", clientID)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); clientWSPingLoop(ctx, c) }()
		go func() {
			defer wg.Done()
			select {
			case <-supersede:
				cancel()
			case <-ctx.Done():
			}
		}()

		clientWSReadLoop(ctx, c)

		cancel()
		wg.Wait()
		presence.Disconnect(tok)
		c.Close(websocket.StatusNormalClosure, "")
	}
}
