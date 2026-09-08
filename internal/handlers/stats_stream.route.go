package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/config"
	"emly-api-go/internal/models"
	"emly-api-go/internal/statshub"
)

// GET /v2/stats/stream: the real-time counterpart to GET /v2/stats/{summary,
// clients,events}, per
// docs/superpowers/specs/2026-09-04-websocket-stats-stream-design.md. A
// small number of long-lived, server-to-server WebSocket connections (the
// dashboard's Next.js process, not one per browser) subscribe to one or more
// channels and get an immediate snapshot plus pushed updates as
// updater_events are ingested and on a periodic tick.

const (
	channelSummary = "stats:summary"
	channelClients = "stats:clients"
	channelEvents  = "stats:events"

	// wsPingInterval/wsIdleTimeout implement the design doc §7 heartbeat:
	// an application-level ping every 30s, and the connection is torn down
	// if nothing at all (not even a client pong) arrives within 90s.
	wsPingInterval = 30 * time.Second
	wsIdleTimeout  = 90 * time.Second
	wsWriteTimeout = 10 * time.Second
)

var validWSChannels = map[string]bool{channelSummary: true, channelClients: true, channelEvents: true}

// wsEnvelopeOut is the server->client message envelope (design doc §5).
type wsEnvelopeOut struct {
	Type    string      `json:"type"`
	Channel string      `json:"channel,omitempty"`
	TS      string      `json:"ts,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

// wsClientMessage is the client->server envelope: subscribe/unsubscribe/
// ping/pong all fit this one shape, with unused fields simply absent.
type wsClientMessage struct {
	Type     string             `json:"type"`
	Channels []string           `json:"channels"`
	Params   *wsSubscribeParams `json:"params"`
}

type wsSubscribeParams struct {
	WindowMinutes *int            `json:"window_minutes"`
	Product       *string         `json:"product"`
	Events        *wsEventsParams `json:"events"`
}

type wsEventsParams struct {
	Bucket    *string `json:"bucket"`
	EventType *string `json:"event_type"`
	From      *string `json:"from"`
	To        *string `json:"to"`
}

// wsSubData is one connection's current subscription state: which channels
// it wants, and the params that shape their snapshots/updates. It carries no
// lock itself - wsConn.sub is guarded by wsConn.subMu, and subSnapshot hands
// callers a plain copy to read without holding that lock across a query.
type wsSubData struct {
	channels        map[string]bool
	windowMinutes   int
	product         string
	eventsBucket    string
	eventsEventType string
	eventsFrom      time.Time
	eventsTo        time.Time
}

// wsConn is one accepted /v2/stats/stream connection.
type wsConn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex

	subMu sync.Mutex
	sub   wsSubData
}

func newWSConn(ws *websocket.Conn) *wsConn {
	now := time.Now().UTC()
	return &wsConn{
		ws: ws,
		sub: wsSubData{
			channels:      make(map[string]bool),
			windowMinutes: defaultConnectedWindowMinutes,
			product:       "emly",
			eventsBucket:  "day",
			eventsFrom:    now.AddDate(0, 0, -30),
			eventsTo:      now,
		},
	}
}

// subSnapshot returns a copy of the connection's current subscription state,
// safe to read without holding subMu across a DB query or a send.
func (cn *wsConn) subSnapshot() wsSubData {
	cn.subMu.Lock()
	defer cn.subMu.Unlock()
	cp := cn.sub
	cp.channels = make(map[string]bool, len(cn.sub.channels))
	for c, v := range cn.sub.channels {
		cp.channels[c] = v
	}
	return cp
}

// send marshals and writes one envelope, bounding the write with its own
// short timeout derived from ctx so a stuck connection can't wedge a caller
// (in particular the hub event loop, which must stay responsive to other
// connections... well, there's one hub loop per connection, but this still
// keeps a single slow write from hanging the whole goroutine indefinitely).
func (cn *wsConn) send(ctx context.Context, typ, channel string, data interface{}) error {
	env := wsEnvelopeOut{Type: typ, Channel: channel, TS: time.Now().UTC().Format(time.RFC3339), Data: data}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}

	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()

	cn.writeMu.Lock()
	defer cn.writeMu.Unlock()
	return cn.ws.Write(wctx, websocket.MessageText, b)
}

func (cn *wsConn) sendError(ctx context.Context, code, message string) {
	_ = cn.send(ctx, "error", "", map[string]string{"code": code, "message": message})
}

// sendSnapshot fetches and sends the full current state of one channel -
// used both right after a subscribe (design doc §5.1) and as the tick-driven
// resync for stats:clients (see handleTick).
func (cn *wsConn) sendSnapshot(ctx context.Context, db *sqlx.DB, channel string) {
	switch channel {
	case channelSummary:
		s := cn.subSnapshot()
		summary, err := fetchStatsSummary(ctx, db, s.windowMinutes, s.product)
		if err != nil {
			cn.sendError(ctx, "internal", "failed to fetch stats summary")
			return
		}
		_ = cn.send(ctx, "snapshot", channelSummary, summary)

	case channelClients:
		clients, err := fetchAllStatsClients(ctx, db)
		if err != nil {
			cn.sendError(ctx, "internal", "failed to fetch clients")
			return
		}
		_ = cn.send(ctx, "snapshot", channelClients, map[string]interface{}{"clients": clients})

	case channelEvents:
		s := cn.subSnapshot()
		resp, err := fetchStatsEvents(ctx, db, s.eventsBucket, s.eventsEventType, s.product, s.eventsFrom, s.eventsTo)
		if err != nil {
			cn.sendError(ctx, "internal", "failed to fetch events")
			return
		}
		_ = cn.send(ctx, "snapshot", channelEvents, resp)
	}
}

// handleSubscribe applies a subscribe message: unknown channels and
// malformed params are reported as individual `error` messages (design doc
// §10 checklist) without dropping the rest of the request, channels are
// added to (never replace) the connection's subscription set - unsubscribe
// is the only way to remove one - and every channel named in *this* message
// gets an immediate snapshot, even if the connection was already subscribed
// to it (that's how a client applies a changed filter, e.g. a new events
// bucket, without reconnecting).
func (cn *wsConn) handleSubscribe(ctx context.Context, db *sqlx.DB, msg wsClientMessage) {
	var validChannels, invalidChannels []string
	for _, c := range msg.Channels {
		if validWSChannels[c] {
			validChannels = append(validChannels, c)
		} else {
			invalidChannels = append(invalidChannels, c)
		}
	}
	if len(invalidChannels) > 0 {
		cn.sendError(ctx, "invalid_params", "unknown channel(s): "+strings.Join(invalidChannels, ", "))
	}

	var paramErrs []string

	cn.subMu.Lock()
	for _, c := range validChannels {
		cn.sub.channels[c] = true
	}
	if p := msg.Params; p != nil {
		if p.WindowMinutes != nil {
			if *p.WindowMinutes > 0 {
				cn.sub.windowMinutes = *p.WindowMinutes
			} else {
				paramErrs = append(paramErrs, "window_minutes must be > 0")
			}
		}
		if p.Product != nil {
			if _, _, _, ok := productFilter(*p.Product); ok {
				product := *p.Product
				if product == "" {
					product = "emly"
				}
				cn.sub.product = product
			} else {
				paramErrs = append(paramErrs, "product must be one of: emly, updater, all")
			}
		}
		if ev := p.Events; ev != nil {
			if ev.Bucket != nil {
				if validEventBuckets[*ev.Bucket] {
					cn.sub.eventsBucket = *ev.Bucket
				} else {
					paramErrs = append(paramErrs, "events.bucket must be one of: day, hour")
				}
			}
			if ev.EventType != nil {
				cn.sub.eventsEventType = *ev.EventType
			}
			if ev.From != nil {
				if t, err := time.Parse(time.RFC3339, *ev.From); err == nil {
					cn.sub.eventsFrom = t
				} else {
					paramErrs = append(paramErrs, "events.from must be RFC3339")
				}
			}
			if ev.To != nil {
				if t, err := time.Parse(time.RFC3339, *ev.To); err == nil {
					cn.sub.eventsTo = t
				} else {
					paramErrs = append(paramErrs, "events.to must be RFC3339")
				}
			}
		}
	}
	channels := make([]string, 0, len(cn.sub.channels))
	for c := range cn.sub.channels {
		channels = append(channels, c)
	}
	cn.subMu.Unlock()

	for _, e := range paramErrs {
		cn.sendError(ctx, "invalid_params", e)
	}

	sort.Strings(channels)
	_ = cn.send(ctx, "subscribed", "", map[string]interface{}{"channels": channels})

	for _, c := range validChannels {
		cn.sendSnapshot(ctx, db, c)
	}
}

func (cn *wsConn) handleUnsubscribe(msg wsClientMessage) {
	cn.subMu.Lock()
	for _, c := range msg.Channels {
		delete(cn.sub.channels, c)
	}
	cn.subMu.Unlock()
}

func (cn *wsConn) handleClientMessage(ctx context.Context, db *sqlx.DB, raw []byte) {
	var msg wsClientMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError(ctx, "invalid_params", "invalid JSON")
		return
	}

	switch msg.Type {
	case "subscribe":
		cn.handleSubscribe(ctx, db, msg)
	case "unsubscribe":
		cn.handleUnsubscribe(msg)
	case "ping":
		_ = cn.send(ctx, "pong", "", nil)
	case "pong":
		// No action needed: reading anything at all already reset the idle
		// timer in readLoop.
	default:
		cn.sendError(ctx, "invalid_params", "unknown message type: "+msg.Type)
	}
}

// wsEventMatchesSubscription reports whether an ingested updater_events row
// falls inside a stats:events subscription's current filter, so the events
// channel is only recomputed and pushed when it could actually change what
// that connection sees (design doc §6.1: "il cui bucket copre il timestamp
// dell'evento").
func wsEventMatchesSubscription(ev *models.UpdaterEvent, s wsSubData) bool {
	if s.eventsEventType != "" && ev.EventType != s.eventsEventType {
		return false
	}
	if s.product != "all" && ev.Product != s.product {
		return false
	}
	if ev.CreatedAt.Before(s.eventsFrom) || ev.CreatedAt.After(s.eventsTo) {
		return false
	}
	return true
}

// handleUpdaterEventPush reacts to one ingested updater_events row (design
// doc §6.1). stats:clients never needs a query here - the hub already
// carries the fresh client row recordUpdaterEvent fetched at ingest time.
func (cn *wsConn) handleUpdaterEventPush(ctx context.Context, db *sqlx.DB, ev statshub.Event) {
	s := cn.subSnapshot()

	if s.channels[channelSummary] {
		if summary, err := fetchStatsSummary(ctx, db, s.windowMinutes, s.product); err == nil {
			_ = cn.send(ctx, "update", channelSummary, summary)
		}
	}

	if s.channels[channelClients] && ev.Client != nil {
		_ = cn.send(ctx, "update", channelClients, map[string]interface{}{
			"upserted":    []models.UpdaterClient{*ev.Client},
			"removed_ids": []int{},
		})
	}

	if s.channels[channelEvents] && ev.EventEntry != nil && wsEventMatchesSubscription(ev.EventEntry, s) {
		if resp, err := fetchStatsEvents(ctx, db, s.eventsBucket, s.eventsEventType, s.product, s.eventsFrom, s.eventsTo); err == nil {
			_ = cn.send(ctx, "update", channelEvents, resp)
		}
	}
}

// handleTick reacts to the hub's periodic EventKindTick (design doc §6.2):
// connected_clients (part of stats:summary) and each client's derived
// "online" state (which the dashboard computes from last_seen_at - see the
// design doc's implementation notes) both go stale purely from time passing,
// with no new updater_events row to trigger a push. stats:clients has no
// per-tick delta to compute, so it gets a full resnapshot instead - cheap at
// the fleet sizes this serves (design doc §1).
func (cn *wsConn) handleTick(ctx context.Context, db *sqlx.DB) {
	s := cn.subSnapshot()

	if s.channels[channelSummary] {
		if summary, err := fetchStatsSummary(ctx, db, s.windowMinutes, s.product); err == nil {
			_ = cn.send(ctx, "update", channelSummary, summary)
		}
	}
	if s.channels[channelClients] {
		cn.sendSnapshot(ctx, db, channelClients)
	}
}

func (cn *wsConn) handleHubEvent(ctx context.Context, db *sqlx.DB, ev statshub.Event) {
	switch ev.Kind {
	case statshub.EventKindUpdaterEvent:
		cn.handleUpdaterEventPush(ctx, db, ev)
	case statshub.EventKindTick:
		cn.handleTick(ctx, db)
	}
}

// readLoop is the connection's single reader (coder/websocket requires reads
// to be sequential). Each Read is bounded by wsIdleTimeout, so the loop -
// and with it the whole connection, per the deferred close in StatsStream -
// exits the moment 90s pass without a single frame from the client,
// application ping/pong included (design doc §7).
func (cn *wsConn) readLoop(ctx context.Context, db *sqlx.DB) {
	for {
		rctx, cancel := context.WithTimeout(ctx, wsIdleTimeout)
		_, data, err := cn.ws.Read(rctx)
		cancel()
		if err != nil {
			return
		}
		cn.handleClientMessage(ctx, db, data)
	}
}

// pingLoop sends the application-level heartbeat (design doc §7). It exits
// on ctx cancellation (normal teardown, driven by readLoop returning) or the
// first failed write (a connection problem readLoop's next Read will also
// observe and exit on).
func (cn *wsConn) pingLoop(ctx context.Context) {
	t := time.NewTicker(wsPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := cn.send(ctx, "ping", "", nil); err != nil {
				return
			}
		}
	}
}

// eventLoop subscribes to hub and pushes whatever it publishes through to
// this connection, filtered by its current subscriptions. A nil hub (not
// expected outside tests) just idles until the connection closes, same as a
// hub with nobody publishing to it.
func (cn *wsConn) eventLoop(ctx context.Context, hub *statshub.Hub, db *sqlx.DB) {
	if hub == nil {
		<-ctx.Done()
		return
	}
	ch, unsubscribe := hub.Subscribe()
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			cn.handleHubEvent(ctx, db, ev)
		}
	}
}

// StatsStream handles GET /v2/stats/stream (design doc §3/§4). Auth is
// checked before the WebSocket upgrade completes: X-Admin-Key (or, only as a
// fallback for a proxy that strips custom headers on the Upgrade request,
// ?admin_key=) must match the configured admin key, or the request is
// answered 401 with no upgrade attempted at all - so a client can tell "bad
// key" apart from "network/server problem" immediately, and nothing about
// the connection is exposed to an unauthenticated caller.
func StatsStream(db *sqlx.DB, hub *statshub.Hub) http.HandlerFunc {
	cfg := config.Load()

	return func(w http.ResponseWriter, r *http.Request) {
		adminKey := r.Header.Get("X-Admin-Key")
		if adminKey == "" {
			adminKey = r.URL.Query().Get("admin_key")
		}
		if cfg.AdminKey == "" || adminKey != cfg.AdminKey {
			jsonError(w, http.StatusUnauthorized, "unauthorized admin key")
			slog.WarnContext(r.Context(), "stats stream: admin key auth failed", "url", r.URL.Path)
			return
		}

		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			slog.WarnContext(r.Context(), "stats stream: upgrade failed", "error", err)
			return
		}

		cn := newWSConn(c)

		ctx, cancel := context.WithCancel(r.Context())

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cn.pingLoop(ctx) }()
		go func() { defer wg.Done(); cn.eventLoop(ctx, hub, db) }()

		cn.readLoop(ctx, db)

		cancel()
		wg.Wait()
		c.Close(websocket.StatusNormalClosure, "")
	}
}
