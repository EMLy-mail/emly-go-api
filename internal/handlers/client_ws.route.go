package handlers

import (
	"encoding/json"
	"net/http"
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
