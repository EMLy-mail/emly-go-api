package models

import "time"

type Session struct {
	ID        string    `db:"id"         json:"id"`
	UserID    string    `db:"user_id"    json:"user_id"`
	ExpiresAt time.Time `db:"expires_at" json:"expires_at"`
}
