package models

import "time"

type Release struct {
	ID                 int       `db:"id"                   json:"-"`
	Product            string    `db:"product"               json:"product"`
	Version            string    `db:"version"              json:"version"`
	Channel            string    `db:"channel"              json:"channel"`
	DownloadFilename   string    `db:"download_filename"    json:"download_filename"`
	SHA256Checksum     string    `db:"sha256_checksum"      json:"sha256_checksum"`
	ShortNote          string    `db:"short_note"           json:"short_note"`
	SeverityType       string    `db:"severity_type"        json:"severity_type"`
	DescriptionEN      *string   `db:"description_en"       json:"description_en,omitempty"`
	DescriptionIT      *string   `db:"description_it"       json:"description_it,omitempty"`
	IsCritical         bool      `db:"is_critical"          json:"is_critical"`
	CriticalVersion    *string   `db:"critical_version"     json:"critical_version,omitempty"`
	MinRequiredVersion *string   `db:"min_required_version" json:"min_required_version,omitempty"`
	ReleasedAt         time.Time `db:"released_at"          json:"released_at"`
	CreatedAt          time.Time `db:"created_at"           json:"created_at"`
}

type UpdateManifest struct {
	StableVersion        string                  `json:"stableVersion"`
	BetaVersion          string                  `json:"betaVersion,omitempty"`
	StableDownload       string                  `json:"stableDownload"`
	BetaDownload         string                  `json:"betaDownload,omitempty"`
	IsCritical           bool                    `json:"isCritical"`
	CriticalVersion      string                  `json:"criticalVersion,omitempty"`
	MinRequiredVersion   string                  `json:"minRequiredVersion,omitempty"`
	SHA256Checksums      map[string]string       `json:"sha256Checksums"`
	ReleaseNotes         map[string]string       `json:"releaseNotes"`
	DetailedReleaseNotes map[string]DetailedNote `json:"detailedReleaseNotes,omitempty"`
}

type DetailedNote struct {
	SeverityType string            `json:"severityType"`
	Description  map[string]string `json:"description"`
}
