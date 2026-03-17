package models

import "time"

type Session struct {
	ID        string    `db:"id"         json:"id"`
	UserID    int64     `db:"user_id"    json:"user_id"`
	Token     string    `db:"token"      json:"token"`
	ExpiresAt time.Time `db:"expires_at" json:"expires_at"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}
