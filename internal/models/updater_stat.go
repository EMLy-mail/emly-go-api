package models

import "time"

type UpdaterClient struct {
	ID       int     `db:"id"                json:"id"`
	HWID     *string `db:"hwid"              json:"hwid,omitempty"`
	Hostname string  `db:"hostname"          json:"hostname"`
	ADDomain string  `db:"ad_domain"         json:"ad_domain"`
	// LoggedUser is the interactive user (console or RDP) seen on the machine
	// at the last sighting that reported one, as `DOMAIN\user`. It is a
	// snapshot rather than history - overwritten on every request carrying
	// X-EMLy-LoggedUser - so it should be read against LastSeenAt. Serial and
	// Product are the firmware's chassis serial and vendor product/SKU
	// number. All three are nil for a client that has never reported one.
	LoggedUser *string `db:"logged_user"       json:"logged_user,omitempty"`
	Serial     *string `db:"serial"            json:"serial,omitempty"`
	Product    *string `db:"product"           json:"product,omitempty"`
	// UpdaterVersion is the version of the EMLy Updater that made the request
	// (read off its User-Agent); EMLyVersion is the version of the EMLy app it
	// maintains (X-EMLy-AppVersion). The two move independently, so a fleet on
	// one current updater can still be spread across several EMLy releases.
	UpdaterVersion  *string    `db:"updater_version"   json:"updater_version,omitempty"`
	EMLyVersion     *string    `db:"emly_version"      json:"emly_version,omitempty"`
	ConfigRevision  *int64     `db:"config_revision"   json:"config_revision,omitempty"`
	ConfigFetchedAt *time.Time `db:"config_fetched_at" json:"config_fetched_at,omitempty"`
	Contact         *string    `db:"contact"           json:"contact,omitempty"`
	LastIP          *string    `db:"last_ip"           json:"last_ip,omitempty"`
	FirstSeenAt     time.Time  `db:"first_seen_at"     json:"first_seen_at"`
	LastSeenAt      time.Time  `db:"last_seen_at"      json:"last_seen_at"`
}

// UpdaterEvent is one client-facing update operation. Product tells apart
// traffic for the EMLy app itself ("emly") from the updater's own self-update
// ("updater"), since both flow through the same endpoints shape.
type UpdaterEvent struct {
	ID        int64     `db:"id"          json:"id"`
	ClientID  int       `db:"client_id"   json:"client_id"`
	EventType string    `db:"event_type"  json:"event_type"`
	Product   string    `db:"product"     json:"product"`
	Version   *string   `db:"version"     json:"version,omitempty"`
	IPAddress *string   `db:"ip_address"  json:"ip_address,omitempty"`
	CreatedAt time.Time `db:"created_at"  json:"created_at"`
}
