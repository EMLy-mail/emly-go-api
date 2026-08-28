package models

import "time"

// UpdaterRelease is one published build of the EMLy Updater itself. Unlike
// Release, there are no channels: at most one row carries is_current = 1 and
// that is the only build ever served. Clearing the flag on every row is the
// kill-switch - the manifest then reports an empty version.
type UpdaterRelease struct {
	ID               int       `db:"id"                json:"-"`
	Version          string    `db:"version"           json:"version"`
	IsCurrent        bool      `db:"is_current"        json:"is_current"`
	DownloadFilename string    `db:"download_filename" json:"download_filename"`
	SHA256Checksum   string    `db:"sha256_checksum"   json:"sha256_checksum"`
	FileSize         int64     `db:"file_size"         json:"file_size"`
	NotesIT          *string   `db:"notes_it"          json:"notes_it,omitempty"`
	NotesEN          *string   `db:"notes_en"          json:"notes_en,omitempty"`
	PublishedAt      time.Time `db:"published_at"      json:"published_at"`
	CreatedAt        time.Time `db:"created_at"        json:"created_at"`
}

// UpdaterManifest is the self-update contract consumed by the EMLy Updater.
// Every field except Version is omitempty, so "nothing to distribute"
// serializes to exactly {"version": ""} - which the client treats as a silent
// no-op. Unknown fields are ignored client-side, so the API may grow new ones
// without breaking updaters already in the field.
type UpdaterManifest struct {
	Version      string            `json:"version"`
	Download     string            `json:"download,omitempty"`
	SHA256       string            `json:"sha256,omitempty"`
	Size         int64             `json:"size,omitempty"`
	PublishedAt  string            `json:"publishedAt,omitempty"`
	ReleaseNotes map[string]string `json:"releaseNotes,omitempty"`
}
