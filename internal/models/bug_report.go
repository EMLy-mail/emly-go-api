package models

import (
	"encoding/json"
	"time"
)

type BugReportStatus string

const (
	BugReportStatusOpen       BugReportStatus = "open"
	BugReportStatusInProgress BugReportStatus = "in_progress"
	BugReportStatusResolved   BugReportStatus = "resolved"
	BugReportStatusClosed     BugReportStatus = "closed"
)

type BugReport struct {
	ID          int64           `db:"id"           json:"id"`
	UserID      *int64          `db:"user_id"      json:"user_id"`
	Title       string          `db:"title"        json:"title"`
	Description string          `db:"description"  json:"description"`
	Status      BugReportStatus `db:"status"       json:"status"`
	SystemInfo  json.RawMessage `db:"system_info"  json:"system_info,omitempty"`
	CreatedAt   time.Time       `db:"created_at"   json:"created_at"`
	UpdatedAt   time.Time       `db:"updated_at"   json:"updated_at"`
}
