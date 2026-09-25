package models

import "time"

type UserRole string

const (
	UserRoleOwner UserRole = "owner"
	UserRoleAdmin UserRole = "admin"
	UserRoleUser  UserRole = "user"
)

// Auth providers a user account can come from.
const (
	AuthProviderLocal = "local"
	AuthProviderOIDC  = "oidc"
)

type User struct {
	ID           string    `db:"id"            json:"id"`
	Username     string    `db:"username"      json:"username"`
	Displayname  string    `db:"displayname"   json:"displayname"`
	PasswordHash string    `db:"password_hash" json:"-"`
	Role         UserRole  `db:"role"          json:"role"`
	Enabled      bool      `db:"enabled"       json:"enabled"`
	AuthProvider string    `db:"auth_provider" json:"auth_provider"`
	CreatedAt    time.Time `db:"created_at"    json:"created_at"`
}
