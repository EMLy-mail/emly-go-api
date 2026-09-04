package models

import "time"

// RemoteConfigRevision is one row of remote_config_revisions: a whole
// /v2/config document as the exact canonical JSON bytes served for it, plus
// the metadata needed to list, publish and roll back revisions. See
// docs/superpowers/specs/2026-09-04-remote-config-api-design.md §4.
type RemoteConfigRevision struct {
	Revision      int64      `db:"revision"       json:"revision"`
	SchemaVersion int        `db:"schema_version"  json:"schema_version"`
	Status        string     `db:"status"          json:"status"`
	Document      string     `db:"document"        json:"document,omitempty"`
	ETag          string     `db:"etag"            json:"etag"`
	Notes         *string    `db:"notes"           json:"notes,omitempty"`
	CreatedBy     *string    `db:"created_by"      json:"created_by,omitempty"`
	BasedOn       *int64     `db:"based_on"        json:"based_on,omitempty"`
	GeneratedAt   time.Time  `db:"generated_at"    json:"generated_at"`
	PublishedAt   *time.Time `db:"published_at"   json:"published_at,omitempty"`
	CreatedAt     time.Time  `db:"created_at"      json:"created_at"`
}

// RemoteConfigRevisionSummary is the metadata-only projection served by the
// revisions list (API design doc §7.1): everything except the document
// body, plus a rollout count computed from updater_clients.
type RemoteConfigRevisionSummary struct {
	Revision          int64      `db:"revision"            json:"revision"`
	SchemaVersion     int        `db:"schema_version"      json:"schema_version"`
	Status            string     `db:"status"              json:"status"`
	ETag              string     `db:"etag"                json:"etag"`
	Notes             *string    `db:"notes"               json:"notes,omitempty"`
	CreatedBy         *string    `db:"created_by"          json:"created_by,omitempty"`
	BasedOn           *int64     `db:"based_on"            json:"based_on,omitempty"`
	GeneratedAt       time.Time  `db:"generated_at"        json:"generated_at"`
	PublishedAt       *time.Time `db:"published_at"        json:"published_at,omitempty"`
	CreatedAt         time.Time  `db:"created_at"          json:"created_at"`
	ClientsOnRevision int        `db:"clients_on_revision" json:"clients_on_revision"`
}
