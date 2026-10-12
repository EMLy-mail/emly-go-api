package clientlogs

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"emly-api-go/internal/dbvalue"
	"emly-api-go/internal/response"
)

// MountAdmin mounts the log routes on r. r is the /v2/client router behind
// AdminKeyAuth (internal/clientws.RegisterV2 calls it): the logs are the
// admin half of the client channel, like its commands and events.
func MountAdmin(r chi.Router, s *Store) {
	r.Get("/{client_id}/logs", s.listHandler)
	r.Get("/logs/{log_id}", s.getHandler)
	r.Get("/logs/{log_id}/content", s.contentHandler)
	r.Delete("/logs/{log_id}", s.deleteHandler)
}

func (s *Store) ready(w http.ResponseWriter) bool {
	if s == nil || s.db == nil {
		response.Error(w, http.StatusServiceUnavailable, "client log storage unavailable")
		return false
	}
	return true
}

// logIDParam reads {log_id}: a command id, i.e. a 26-character ULID.
func logIDParam(r *http.Request) (string, bool) {
	id := chi.URLParam(r, "log_id")
	return id, validID(id)
}

// validID: 26 characters of Crockford base32, the shape clientproto.NewID
// mints. Anything else cannot be a log id, and is refused before it reaches
// a query or an S3 key.
func validID(id string) bool {
	if len(id) != 26 {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", c) {
			return false
		}
	}
	return true
}

func (s *Store) listHandler(w http.ResponseWriter, r *http.Request) {
	clientID, err := strconv.ParseInt(chi.URLParam(r, "client_id"), 10, 64)
	if err != nil || clientID <= 0 {
		response.Error(w, http.StatusBadRequest, "invalid client_id")
		return
	}
	if !s.ready(w) {
		return
	}
	logs, err := s.List(r.Context(), clientID)
	if err != nil {
		slog.ErrorContext(r.Context(), "listing client logs", "client_id", clientID, "err", err)
		response.Error(w, http.StatusInternalServerError, "failed to list logs")
		return
	}
	response.OK(w, map[string]any{"logs": logs})
}

func (s *Store) getHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := logIDParam(r)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid log_id")
		return
	}
	if !s.ready(w) {
		return
	}
	rec, err := s.Get(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		response.Error(w, http.StatusNotFound, err.Error())
	case err != nil:
		slog.ErrorContext(r.Context(), "reading client log", "log_id", id, "err", err)
		response.Error(w, http.StatusInternalServerError, "failed to read log")
	default:
		response.OK(w, rec)
	}
}

// contentHandler serves the log as a download: the text by default, the
// stored gzip as-is with ?format=gz. The bytes come from a machine, so they
// are always an attachment with nosniff - never something a browser renders.
func (s *Store) contentHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := logIDParam(r)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid log_id")
		return
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "text" && format != "gz" {
		response.Error(w, http.StatusBadRequest, `format must be "text" or "gz"`)
		return
	}
	if !s.ready(w) {
		return
	}
	rec, gz, err := s.Gzip(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		response.Error(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, errBucketUnavailable):
		slog.ErrorContext(r.Context(), "reading client log from the bucket", "log_id", id, "err", err)
		response.Error(w, http.StatusBadGateway, "log storage unavailable")
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "reading client log", "log_id", id, "err", err)
		response.Error(w, http.StatusInternalServerError, "failed to read log")
		return
	}

	name := attachmentName(rec)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	if format == "gz" {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.gz"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(gz)))
		_, _ = w.Write(gz)
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		slog.ErrorContext(r.Context(), "stored client log is not gzip", "log_id", id, "err", err)
		response.Error(w, http.StatusInternalServerError, "stored log is corrupt")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = io.Copy(w, io.LimitReader(zr, MaxDecompressedBytes))
}

func (s *Store) deleteHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := logIDParam(r)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid log_id")
		return
	}
	if !s.ready(w) {
		return
	}
	err := s.Delete(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		response.Error(w, http.StatusNotFound, err.Error())
	case err != nil:
		slog.ErrorContext(r.Context(), "deleting client log", "log_id", id, "err", err)
		response.Error(w, http.StatusInternalServerError, "failed to delete log")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// attachmentName is "<hostname>_<yyyyMMdd-HHmmss>_<file>", the time being
// when the machine read its log (when the API got it, if the machine's clock
// was not usable). Every character outside [A-Za-z0-9._-] becomes '_': the
// hostname and file name come from the machine and end up inside a quoted
// header value.
func attachmentName(rec Record) string {
	host := dbvalue.Deref(rec.Hostname)
	if host == "" {
		host = fmt.Sprintf("client-%d", rec.ClientID)
	}
	at := rec.ReceivedAt
	if rec.CollectedAt != nil {
		at = *rec.CollectedAt
	}
	file := rec.FileName
	if file == "" {
		file = "updater.log"
	}
	return sanitize(host) + "_" + at.UTC().Format("20060102-150405") + "_" + sanitize(file)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "_"
	}
	return out
}
