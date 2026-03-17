package models

import "time"

type RateLimitHWID struct {
	HWID        string    `db:"hwid"       json:"hwid"`
	Requests    int       `db:"requests"   json:"requests"`
	WindowStart time.Time `db:"window_start" json:"window_start"`
}
