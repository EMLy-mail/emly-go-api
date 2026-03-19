package models

import "time"

type UserRole string

const (
	UserRoleAdmin UserRole = "admin"
	UserRoleUser  UserRole = "user"
)

type User struct {
	ID           string    `db:"id"            json:"id"`
	Username     string    `db:"username"      json:"username"`
	Displayname  string    `db:"displayname"   json:"displayname"`
	PasswordHash string    `db:"password_hash" json:"-"`
	Role         UserRole  `db:"role"          json:"role"`
	Enabled      bool      `db:"enabled"       json:"enabled"`
	CreatedAt    time.Time `db:"created_at"    json:"created_at"`
}
