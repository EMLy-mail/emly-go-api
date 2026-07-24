package models

import "time"

type UpdaterClient struct {
	ID             int       `db:"id"              json:"id"`
	Hostname       string    `db:"hostname"        json:"hostname"`
	ADDomain       string    `db:"ad_domain"        json:"ad_domain"`
	UpdaterVersion *string   `db:"updater_version"  json:"updater_version,omitempty"`
	Contact        *string   `db:"contact"          json:"contact,omitempty"`
	LastIP         *string   `db:"last_ip"          json:"last_ip,omitempty"`
	FirstSeenAt    time.Time `db:"first_seen_at"    json:"first_seen_at"`
	LastSeenAt     time.Time `db:"last_seen_at"     json:"last_seen_at"`
}

type UpdaterEvent struct {
	ID        int64     `db:"id"          json:"id"`
	ClientID  int       `db:"client_id"   json:"client_id"`
	EventType string    `db:"event_type"  json:"event_type"`
	Version   *string   `db:"version"     json:"version,omitempty"`
	IPAddress *string   `db:"ip_address"  json:"ip_address,omitempty"`
	CreatedAt time.Time `db:"created_at"  json:"created_at"`
}
