package clientws

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"

	"emly-api-go/internal/clienthub"
	"emly-api-go/internal/clientproto"
	"emly-api-go/internal/response"
)

const (
	defaultCommandTTL = 600 * time.Second
	minTTLSeconds     = 1
	maxTTLSeconds     = 86400
	minReleaseJitter  = 60
	maxIssuedByLen    = 64
)

// mountAdmin mounts the admin half of the client channel on r (already
// scoped to /v2/client and behind AdminKeyAuth by RegisterV2). Split out so
// the handler tests can mount it without auth.
func mountAdmin(r chi.Router, hub *clienthub.Hub) {
	r.Post("/{client_id}/commands", IssueCommand(hub))
	r.Get("/commands/{command_id}", GetCommand(hub))
	r.Get("/{client_id}/events", ListEvents(hub))
	r.Post("/notify", SendNotify(hub))
}

func clientIDParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "client_id"), 10, 64)
	return id, err == nil && id > 0
}

// validIssuedBy rejects an issued_by that could corrupt the local log/Event
// Log line it ends up in (a control or otherwise non-printable rune) or
// that is implausibly long for "who launched this command" (M2). Empty is
// valid: issued_by is optional.
func validIssuedBy(s string) bool {
	if len(s) > maxIssuedByLen {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

type issueRequest struct {
	Name       string          `json:"name"`
	Args       json.RawMessage `json:"args"`
	TTLSeconds int             `json:"ttl_seconds"`
	IssuedBy   string          `json:"issued_by"`
}

// IssueCommand handles POST /v2/client/{client_id}/commands.
func IssueCommand(hub *clienthub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID, ok := clientIDParam(r)
		if !ok {
			response.Error(w, http.StatusBadRequest, "invalid client_id")
			return
		}
		var req issueRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		ttl := defaultCommandTTL
		if req.TTLSeconds != 0 {
			// Bounds-check the raw int *before* converting to a
			// time.Duration (M1): req.TTLSeconds * time.Second overflows
			// int64 well within what still decodes as a plain JSON number
			// (e.g. 9e15), and an overflowed Duration can wrap around to a
			// value that passes a post-conversion range check by
			// coincidence instead of being rejected.
			if req.TTLSeconds < minTTLSeconds || req.TTLSeconds > maxTTLSeconds {
				response.Error(w, http.StatusBadRequest, "ttl_seconds must be between 1 and 86400")
				return
			}
			ttl = time.Duration(req.TTLSeconds) * time.Second
		}
		if !validIssuedBy(req.IssuedBy) {
			response.Error(w, http.StatusBadRequest, "issued_by must be at most 64 characters with no control characters")
			return
		}
		if e := clientproto.ValidateArgs(req.Name, req.Args); e != nil {
			status := http.StatusBadRequest
			if e.Code == clientproto.ErrUnsupportedCommand {
				status = http.StatusUnprocessableEntity
			}
			response.Error(w, status, e.Message)
			return
		}
		rec, err := hub.Issue(r.Context(), clientID, req.Name, req.Args, ttl, req.IssuedBy)
		switch {
		case errors.Is(err, clienthub.ErrOffline):
			response.Error(w, http.StatusConflict, err.Error())
		case errors.Is(err, clienthub.ErrUnsupported):
			response.Error(w, http.StatusUnprocessableEntity, err.Error())
		case err != nil:
			response.Error(w, http.StatusInternalServerError, "failed to send command")
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(rec)
		}
	}
}

// GetCommand handles GET /v2/client/commands/{command_id}.
func GetCommand(hub *clienthub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec, ok := hub.Command(chi.URLParam(r, "command_id"))
		if !ok {
			response.Error(w, http.StatusNotFound, "command not found")
			return
		}
		response.OK(w, rec)
	}
}

// ListEvents handles GET /v2/client/{client_id}/events.
func ListEvents(hub *clienthub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID, ok := clientIDParam(r)
		if !ok {
			response.Error(w, http.StatusBadRequest, "invalid client_id")
			return
		}
		events := hub.Events(clientID)
		if events == nil {
			events = []clienthub.EventRecord{}
		}
		response.OK(w, map[string]any{"events": events})
	}
}

type notifyRequest struct {
	Topic     string          `json:"topic"`
	Payload   json.RawMessage `json:"payload"`
	ClientIDs []int64         `json:"client_ids"`
}

// SendNotify handles POST /v2/client/notify.
func SendNotify(hub *clienthub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req notifyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		payload, msg := validateNotify(req.Topic, req.Payload)
		if msg != "" {
			response.Error(w, http.StatusBadRequest, msg)
			return
		}
		sent := hub.Notify(r.Context(), req.ClientIDs, req.Topic, payload)
		response.OK(w, map[string]int{"sent": sent})
	}
}

func validateNotify(topic string, raw json.RawMessage) (any, string) {
	switch topic {
	case clientproto.TopicReleasePublished:
		var p clientproto.ReleasePublished
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, "invalid release.published payload"
		}
		switch {
		case p.Target != "emly" && p.Target != "updater":
			return nil, `target must be "emly" or "updater"`
		case p.Version == "":
			return nil, "version is required"
		case p.Target == "emly" && p.Channel != "stable" && p.Channel != "beta":
			return nil, `channel must be "stable" or "beta" for target emly`
		case p.JitterSeconds < minReleaseJitter:
			return nil, "jitter_seconds must be >= 60"
		}
		return p, ""
	case clientproto.TopicConfigPublished:
		var p clientproto.ConfigPublished
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, "invalid config.published payload"
		}
		if p.Revision <= 0 || p.JitterSeconds < 0 {
			return nil, "revision must be > 0 and jitter_seconds >= 0"
		}
		return p, ""
	default:
		return nil, "unknown topic"
	}
}
