// Package clienthub is the in-memory state of protocol v2 on
// GET /v2/client/ws (CLIENT_WS_PROTOCOL.md): which updater_clients.id has a
// v2 session open and what it can do, the commands issued to machines and
// their outcome, and the last events each machine sent.
//
// HTTP- and DB-free like internal/presencehub, and subject to the same
// single-instance limit: a command issued on one API replica can only reach
// a machine connected to that replica. Nothing survives a restart - command
// history is a debugging aid for now, not an audit trail (the agent-channel
// spec's persistence is the follow-up).
package clienthub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"emly-api-go/internal/clientproto"
)

// Sender writes one frame to a connection.
type Sender func(ctx context.Context, frame []byte) error

type Status string

const (
	StatusSent     Status = "sent"
	StatusAcked    Status = "acked"
	StatusRejected Status = "rejected"
	StatusDone     Status = "done"
	StatusFailed   Status = "failed"
	StatusTimeout  Status = "timeout"
)

const (
	// recordRetention is how long a finished command stays readable.
	recordRetention = 24 * time.Hour
	// eventRingSize is how many events are kept per client.
	eventRingSize = 50
	// sendTimeout bounds one outbound frame.
	sendTimeout = 10 * time.Second
	// configJitterSeconds is the jitter announced with config.published.
	configJitterSeconds = 120
)

// ackGrace is ack_timeout_seconds plus slack for the round trip. Not a
// const: clientproto.DefaultLimits is a package var, not a constant.
var ackGrace = time.Duration(clientproto.DefaultLimits.AckTimeoutSeconds)*time.Second + 5*time.Second

var (
	ErrOffline     = errors.New("client is not connected")
	ErrUnsupported = errors.New("client does not support this command")
)

type CommandRecord struct {
	ID         string                 `json:"id"`
	ClientID   int64                  `json:"client_id"`
	Name       string                 `json:"name"`
	Args       json.RawMessage        `json:"args,omitempty"`
	IssuedBy   string                 `json:"issued_by,omitempty"`
	IssuedAt   time.Time              `json:"issued_at"`
	ExpiresAt  time.Time              `json:"expires_at"`
	Status     Status                 `json:"status"`
	AckedAt    *time.Time             `json:"acked_at,omitempty"`
	FinishedAt *time.Time             `json:"finished_at,omitempty"`
	Error      *clientproto.ErrorBody `json:"error,omitempty"`
	Result     *clientproto.Result    `json:"result,omitempty"`

	resultTimeout time.Duration
}

type EventRecord struct {
	ID         string          `json:"id,omitempty"`
	ClientID   int64           `json:"client_id"`
	Name       string          `json:"name"`
	ReceivedAt time.Time       `json:"received_at"`
	ClientTS   string          `json:"client_ts,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

type session struct {
	gen      uint64
	protocol int
	caps     map[string]bool
	send     Sender
}

type Hub struct {
	mu       sync.Mutex
	now      func() time.Time
	nextGen  uint64
	sessions map[int64]*session
	commands map[string]*CommandRecord
	events   map[int64][]EventRecord
}

// New returns an empty Hub; now is time.Now outside tests.
func New(now func() time.Time) *Hub {
	if now == nil {
		now = time.Now
	}
	return &Hub{
		now:      now,
		sessions: map[int64]*session{},
		commands: map[string]*CommandRecord{},
		events:   map[int64][]EventRecord{},
	}
}

// Attach registers the connection now serving clientID, replacing any
// previous one. The returned detach only removes this very session, so a
// superseded connection's cleanup never unregisters its successor (the
// same rule as presencehub.Token).
func (h *Hub) Attach(clientID int64, protocol int, capabilities []string, send Sender) (detach func()) {
	if h == nil {
		return func() {}
	}
	caps := make(map[string]bool, len(capabilities))
	for _, c := range capabilities {
		caps[c] = true
	}
	h.mu.Lock()
	h.nextGen++
	gen := h.nextGen
	h.sessions[clientID] = &session{gen: gen, protocol: protocol, caps: caps, send: send}
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if s, ok := h.sessions[clientID]; ok && s.gen == gen {
			delete(h.sessions, clientID)
		}
	}
}

// Issue validates and sends a command. args must already have passed
// clientproto.ValidateArgs; Issue re-checks it anyway so no caller can skip
// it.
func (h *Hub) Issue(ctx context.Context, clientID int64, name string, args json.RawMessage, ttl time.Duration, issuedBy string) (CommandRecord, error) {
	if h == nil {
		return CommandRecord{}, ErrOffline
	}
	if e := clientproto.ValidateArgs(name, args); e != nil {
		return CommandRecord{}, errors.New(e.Message)
	}
	h.mu.Lock()
	s, ok := h.sessions[clientID]
	h.mu.Unlock()
	if !ok {
		return CommandRecord{}, ErrOffline
	}
	if s.protocol < clientproto.ProtocolV2 || !s.caps[name] {
		return CommandRecord{}, ErrUnsupported
	}

	now := h.now()
	cmd := clientproto.Command{
		Name:      name,
		Args:      args,
		ExpiresAt: now.Add(ttl).UTC().Format(time.RFC3339),
		IssuedBy:  issuedBy,
	}
	frame, err := clientproto.Frame(clientproto.TypeCommand, "", cmd, now)
	if err != nil {
		return CommandRecord{}, err
	}
	var env clientproto.Envelope
	_ = json.Unmarshal(frame, &env)

	rec := &CommandRecord{
		ID: env.ID, ClientID: clientID, Name: name, Args: args, IssuedBy: issuedBy,
		IssuedAt: now, ExpiresAt: now.Add(ttl), Status: StatusSent,
		resultTimeout: clientproto.ResultTimeout(name, args),
	}
	// Registered before the send, so an ack racing the write finds it.
	h.mu.Lock()
	h.commands[rec.ID] = rec
	h.mu.Unlock()

	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := s.send(sctx, frame); err != nil {
		h.mu.Lock()
		delete(h.commands, rec.ID)
		h.mu.Unlock()
		return CommandRecord{}, ErrOffline
	}
	// rec was registered (and reachable from HandleAck/HandleResult/Command/
	// Prune) before send; the send itself can race a client's ack arriving
	// before Issue returns, so the final read must take the lock too, same
	// as Command does.
	h.mu.Lock()
	result := h.snapshot(rec)
	h.mu.Unlock()
	return result, nil
}

// ownCommand returns the record replyTo names if it belongs to clientID.
// Caller holds h.mu.
func (h *Hub) ownCommand(clientID int64, replyTo string) (*CommandRecord, bool) {
	rec, ok := h.commands[replyTo]
	if !ok || rec.ClientID != clientID {
		return nil, false
	}
	return rec, true
}

func (h *Hub) HandleAck(clientID int64, replyTo string, ack clientproto.Ack) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.ownCommand(clientID, replyTo)
	if !ok || rec.Status != StatusSent {
		return
	}
	now := h.now()
	if ack.Accepted {
		rec.Status, rec.AckedAt = StatusAcked, &now
		return
	}
	rec.Status, rec.FinishedAt, rec.Error = StatusRejected, &now, ack.Error
}

func (h *Hub) HandleResult(clientID int64, replyTo string, res clientproto.Result) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.ownCommand(clientID, replyTo)
	if !ok || (rec.Status != StatusSent && rec.Status != StatusAcked) {
		return
	}
	now := h.now()
	rec.FinishedAt, rec.Result = &now, &res
	if res.Status == clientproto.ResultOK {
		rec.Status = StatusDone
	} else {
		rec.Status, rec.Error = StatusFailed, res.Error
	}
}

// CompleteByRestart closes the result-via-event commands a service.started
// event lists (§8.6).
func (h *Hub) CompleteByRestart(clientID int64, ids []string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	for _, id := range ids {
		rec, ok := h.ownCommand(clientID, id)
		if !ok || !clientproto.Commands[rec.Name].ResultViaEvent {
			continue
		}
		if rec.Status == StatusSent || rec.Status == StatusAcked || rec.Status == StatusTimeout {
			rec.Status, rec.FinishedAt, rec.Error = StatusDone, &now, nil
		}
	}
}

// Command returns a copy of a record, with timeouts applied lazily: the
// state a reader sees is always current without a sweeper goroutine.
func (h *Hub) Command(id string) (CommandRecord, bool) {
	if h == nil {
		return CommandRecord{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.commands[id]
	if !ok {
		return CommandRecord{}, false
	}
	h.applyTimeout(rec)
	return h.snapshot(rec), true
}

// applyTimeout marks rec timed out when its deadline passed. Caller holds
// h.mu (or owns rec exclusively).
func (h *Hub) applyTimeout(rec *CommandRecord) {
	now := h.now()
	var deadline time.Time
	switch rec.Status {
	case StatusSent:
		deadline = rec.IssuedAt.Add(ackGrace)
	case StatusAcked:
		deadline = rec.AckedAt.Add(rec.resultTimeout)
	default:
		return
	}
	if now.After(deadline) {
		rec.Status, rec.FinishedAt = StatusTimeout, &now
		rec.Error = &clientproto.ErrorBody{Code: clientproto.ErrTimeout}
	}
}

func (h *Hub) snapshot(rec *CommandRecord) CommandRecord {
	c := *rec
	return c
}

func (h *Hub) RecordEvent(ev EventRecord) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ring := append(h.events[ev.ClientID], ev)
	if len(ring) > eventRingSize {
		ring = ring[len(ring)-eventRingSize:]
	}
	h.events[ev.ClientID] = ring
}

// Events returns the client's recent events, oldest first.
func (h *Hub) Events(clientID int64) []EventRecord {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]EventRecord(nil), h.events[clientID]...)
}

// Notify sends a notify to the listed clients (nil = every v2 session)
// that declared the topic, and returns how many it reached.
func (h *Hub) Notify(ctx context.Context, clientIDs []int64, topic string, payload any) int {
	if h == nil {
		return 0
	}
	frame, err := clientproto.Frame(clientproto.TypeNotify, "", clientproto.Notify{Topic: topic, Payload: payload}, h.now())
	if err != nil {
		return 0
	}
	h.mu.Lock()
	var targets []Sender
	pick := func(s *session) {
		if s.protocol >= clientproto.ProtocolV2 && s.caps[topic] {
			targets = append(targets, s.send)
		}
	}
	if clientIDs == nil {
		for _, s := range h.sessions {
			pick(s)
		}
	} else {
		for _, id := range clientIDs {
			if s, ok := h.sessions[id]; ok {
				pick(s)
			}
		}
	}
	h.mu.Unlock()

	sent := 0
	for _, send := range targets {
		sctx, cancel := context.WithTimeout(ctx, sendTimeout)
		if send(sctx, frame) == nil {
			sent++
		}
		cancel()
	}
	return sent
}

// NotifyConfigPublished satisfies configapi.ConfigNotifier.
func (h *Hub) NotifyConfigPublished(revision int64) {
	n := h.Notify(context.Background(), nil, clientproto.TopicConfigPublished,
		clientproto.ConfigPublished{Revision: revision, JitterSeconds: configJitterSeconds})
	slog.Info("client ws: config.published sent", "revision", revision, "clients", n)
}

// Prune drops finished commands older than recordRetention and events of
// clients with nothing newer than that. Called from a ticker in main.
func (h *Hub) Prune() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := h.now().Add(-recordRetention)
	for id, rec := range h.commands {
		h.applyTimeout(rec)
		if rec.FinishedAt != nil && rec.FinishedAt.Before(cutoff) {
			delete(h.commands, id)
		}
	}
	for id, ring := range h.events {
		if len(ring) > 0 && ring[len(ring)-1].ReceivedAt.Before(cutoff) {
			delete(h.events, id)
		}
	}
}
