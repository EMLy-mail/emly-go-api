package clientproto

import (
	"encoding/json"
	"time"
)

// Protocol versions (CLIENT_WS_PROTOCOL.md §4). A client that sends no
// "protocol" in its identity is v1.
const (
	ProtocolV1      = 1
	ProtocolV2      = 2
	ProtocolCurrent = ProtocolV2
)

// Message types (§5).
const (
	TypeHello    = "hello"
	TypeIdentity = "identity"
	TypePing     = "ping"
	TypePong     = "pong"
	TypeError    = "error"
	TypeWelcome  = "welcome"
	TypeCommand  = "command"
	TypeAck      = "ack"
	TypeResult   = "result"
	TypeEvent    = "event"
	TypeNotify   = "notify"
)

// Command names (§7).
const (
	CmdMachineInfo          = "machine.info"
	CmdEMLyManifestCheck    = "emly.manifest.check"
	CmdUpdaterManifestCheck = "updater.manifest.check"
	CmdAppsListUpgradable   = "apps.list_upgradable"
	CmdServiceRestart       = "service.restart"
	CmdMachineReboot        = "machine.reboot"
)

// Event names (§8).
const (
	EvtMachineInfo     = "machine.info"
	EvtSessionChanged  = "session.changed"
	EvtUpdateAvailable = "update.available"
	EvtUpdateStarted   = "update.started"
	EvtUpdateApplied   = "update.applied"
	EvtUpdateFailed    = "update.failed"
	EvtServiceStarted  = "service.started"
)

// Notify topics (§9).
const (
	TopicReleasePublished = "release.published"
	TopicConfigPublished  = "config.published"
)

// Error codes (§10).
const (
	ErrUnidentified       = "unidentified"
	ErrProtocol           = "protocol_error"
	ErrMessageTooLarge    = "message_too_large"
	ErrRateLimited        = "rate_limited"
	ErrUnsupportedCommand = "unsupported_command"
	ErrInvalidArgs        = "invalid_args"
	ErrExpired            = "expired"
	ErrBusy               = "busy"
	ErrDisabledByPolicy   = "disabled_by_policy"
	ErrInsecureTransport  = "insecure_transport"
	ErrUserActive         = "user_active"
	ErrTimeout            = "timeout"
	ErrInternal           = "internal"
)

// Envelope is every frame on the channel (§3). Only Type is mandatory; the
// v1 bytes {"type":"ping"} are a valid Envelope.
type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	ReplyTo string          `json:"reply_to,omitempty"`
	TS      string          `json:"ts,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// tsLayout is RFC 3339 UTC with milliseconds, the "ts" format of §3.
const tsLayout = "2006-01-02T15:04:05.000Z07:00"

// Frame marshals one message. ping and pong keep the exact v1 bytes (no
// id, no ts); every other type gets a fresh ULID and a timestamp. data nil
// omits "data".
func Frame(typ, replyTo string, data any, now time.Time) ([]byte, error) {
	env := Envelope{Type: typ, ReplyTo: replyTo}
	if typ != TypePing && typ != TypePong {
		env.ID = NewID()
		env.TS = now.UTC().Format(tsLayout)
	}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		env.Data = raw
	}
	return json.Marshal(env)
}

// Hello is hello.data in v2 (§4). A v1 client ignores it.
type Hello struct {
	Protocol      int    `json:"protocol"`
	ServerVersion string `json:"server_version,omitempty"`
	ServerTime    string `json:"server_time"`
}

// IdentityExt is the part of identity.data that v2 adds on top of the v1
// fields (updaterclient.WSIdentityPayload).
type IdentityExt struct {
	Protocol     int      `json:"protocol,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Limits is welcome.data.limits (§11).
type Limits struct {
	MaxMessageBytes    int `json:"max_message_bytes"`
	MaxEventsPerMinute int `json:"max_events_per_minute"`
	AckTimeoutSeconds  int `json:"ack_timeout_seconds"`
}

var DefaultLimits = Limits{MaxMessageBytes: 65536, MaxEventsPerMinute: 60, AckTimeoutSeconds: 5}

type Welcome struct {
	Protocol             int      `json:"protocol"`
	AcceptedCapabilities []string `json:"accepted_capabilities"`
	Limits               Limits   `json:"limits"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type Command struct {
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args,omitempty"`
	ExpiresAt string          `json:"expires_at"`
	IssuedBy  string          `json:"issued_by,omitempty"`
}

type Ack struct {
	Accepted  bool       `json:"accepted"`
	Error     *ErrorBody `json:"error,omitempty"`
	Duplicate bool       `json:"duplicate,omitempty"`
}

// Result statuses.
const (
	ResultOK    = "ok"
	ResultError = "error"
)

type Result struct {
	Status     string          `json:"status"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Error      *ErrorBody      `json:"error,omitempty"`
	Truncated  bool            `json:"truncated,omitempty"`
}

type Event struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Notify struct {
	Topic   string `json:"topic"`
	Payload any    `json:"payload,omitempty"`
}

// UserSession is §6.1.
type UserSession struct {
	User           string `json:"user,omitempty"`
	State          string `json:"state,omitempty"`
	DisconnectedAt string `json:"disconnected_at,omitempty"`
}

// SessionChanged is the payload of event session.changed (§8.1).
type SessionChanged struct {
	Events      []string     `json:"events"`
	SessionID   uint32       `json:"session_id"`
	SessionUser string       `json:"session_user,omitempty"`
	LoggedUser  *UserSession `json:"logged_user,omitempty"`
	Changed     bool         `json:"changed"`
	At          string       `json:"at,omitempty"`
}

// ServiceStarted is the payload of event service.started (§8.6).
type ServiceStarted struct {
	Reason            string   `json:"reason"`
	BootTime          string   `json:"boot_time,omitempty"`
	StartedAt         string   `json:"started_at,omitempty"`
	UpdaterVersion    string   `json:"updater_version,omitempty"`
	PreviousVersion   string   `json:"previous_version,omitempty"`
	CompletedCommands []string `json:"completed_commands,omitempty"`
}

// ReleasePublished / ConfigPublished are the notify payloads of §9.
type ReleasePublished struct {
	Target        string `json:"target"`
	Channel       string `json:"channel,omitempty"`
	Version       string `json:"version"`
	JitterSeconds int    `json:"jitter_seconds"`
}

type ConfigPublished struct {
	Revision      int64 `json:"revision"`
	JitterSeconds int   `json:"jitter_seconds"`
}

// CommandSpec is what the server needs to know about a verb to track it.
type CommandSpec struct {
	// ResultTimeout is measured from the ack. For ResultViaEvent verbs it
	// bounds the wait for service.started instead.
	ResultTimeout time.Duration
	// Destructive verbs need the admin to confirm (dashboard) and a wss://
	// connection on the client (§12.2).
	Destructive bool
	// ResultViaEvent verbs never send a result: the process dies first, and
	// completion is service.started listing the command id (§5.3).
	ResultViaEvent bool
}

var Commands = map[string]CommandSpec{
	CmdMachineInfo:          {ResultTimeout: 30 * time.Second},
	CmdEMLyManifestCheck:    {ResultTimeout: 60 * time.Second},
	CmdUpdaterManifestCheck: {ResultTimeout: 60 * time.Second},
	CmdAppsListUpgradable:   {ResultTimeout: 180 * time.Second},
	CmdServiceRestart:       {ResultTimeout: 120 * time.Second, Destructive: true, ResultViaEvent: true},
	CmdMachineReboot:        {ResultTimeout: 15 * time.Minute, Destructive: true, ResultViaEvent: true},
}

// ServerCapabilities is every command, event and topic this server
// understands; welcome.accepted_capabilities is the client's list
// intersected with it.
var ServerCapabilities = []string{
	CmdMachineInfo, CmdEMLyManifestCheck, CmdUpdaterManifestCheck,
	CmdAppsListUpgradable, CmdServiceRestart, CmdMachineReboot,
	EvtMachineInfo, EvtSessionChanged, EvtUpdateAvailable, EvtUpdateStarted,
	EvtUpdateApplied, EvtUpdateFailed, EvtServiceStarted,
	TopicReleasePublished, TopicConfigPublished,
}

// Intersect returns the entries of offered that are in supported, in
// offered's order, without duplicates.
func Intersect(offered, supported []string) []string {
	ok := make(map[string]bool, len(supported))
	for _, s := range supported {
		ok[s] = true
	}
	out := []string{}
	for _, s := range offered {
		if ok[s] {
			out = append(out, s)
			delete(ok, s)
		}
	}
	return out
}
