package clientws

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/response"
	"emly-api-go/internal/updaterclient"
)

// clientDataMaxBody bounds the POST /v2/clients/data body. The payload is a
// dozen short strings; anything near this size is not an updater.
const clientDataMaxBody = 64 << 10

// PostClientData handles POST /v2/clients/data: the EMLy Updater reporting
// the same facts it sends as X-EMLy-* headers on every manifest/download
// request, as one JSON body instead - without having to check for a release
// or hold GET /v2/client/ws open to do it.
//
// The body is updaterclient.WSIdentityPayload, the exact shape of the WS
// "identity" message, and goes through IdentityFromWSPayload into the same
// Upsert: a third wire format for the same facts must not become a third
// identity constructor. As on the WS, updater_version/contact come from the
// User-Agent and the IP from the connection, never from the body.
//
// It is a sighting, not an operation: the client row (and last_seen_at) is
// updated, but no updater_events row is written - event_type only knows
// manifest_check and download, and the fleet charts count those.
//
// 204 on success and for X-EMLy-Testing traffic (served, never recorded,
// same as every other updater path); 400 for a body that is not JSON or
// names no machine - unlike a manifest check, identifying the machine is
// this route's whole purpose, so silently accepting it would hide a broken
// client.
func PostClientData(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, clientDataMaxBody)
		var payload updaterclient.WSIdentityPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				response.Error(w, http.StatusRequestEntityTooLarge, "body too large")
				return
			}
			response.Error(w, http.StatusBadRequest, "invalid JSON body")
			return
		}

		id := updaterclient.IdentityFromWSPayload(r, payload)
		if !id.Identified() {
			response.Error(w, http.StatusBadRequest, "body carries neither hwid nor hostname")
			return
		}

		if updaterclient.IsTestTraffic(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if _, err := upsertClientFn(r.Context(), db, id); err != nil {
			slog.ErrorContext(r.Context(), "client data: failed to upsert client", "error", err)
			response.Error(w, http.StatusInternalServerError, "failed to record client data")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
