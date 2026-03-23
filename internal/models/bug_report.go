package models

import (
	"encoding/json"
	"strings"
	"time"
)

type BugReportStatus string

const (
	BugReportStatusNew      BugReportStatus = "new"
	BugReportStatusInReview BugReportStatus = "in_review"
	BugReportStatusResolved BugReportStatus = "resolved"
	BugReportStatusClosed   BugReportStatus = "closed"
)

func (s BugReportStatus) IsValid() bool {
	switch s {
	case BugReportStatusNew, BugReportStatusInReview, BugReportStatusResolved, BugReportStatusClosed:
		return true
	default:
		return false
	}
}

func ParseBugReportStatus(value string) (BugReportStatus, bool) {
	status := BugReportStatus(strings.ToLower(strings.TrimSpace(value)))
	if status == "" {
		return "", false
	}
	return status, status.IsValid()
}

type BugReportListItem struct {
	BugReport
	FileCount int `db:"file_count" json:"file_count"`
}

type BugReport struct {
	ID          uint64          `db:"id"           json:"id"`
	Name        string          `db:"name"         json:"name"`
	Email       string          `db:"email"        json:"email"`
	Description string          `db:"description"  json:"description"`
	HWID        string          `db:"hwid"         json:"hwid"`
	Hostname    string          `db:"hostname"     json:"hostname"`
	OsUser      string          `db:"os_user"      json:"os_user"`
	SubmitterIP string          `db:"submitter_ip" json:"submitter_ip"`
	SystemInfo  json.RawMessage `db:"system_info"  json:"system_info,omitempty"`
	Status      BugReportStatus `db:"status"       json:"status"`
	CreatedAt   time.Time       `db:"created_at"   json:"created_at"`
	UpdatedAt   time.Time       `db:"updated_at"   json:"updated_at"`
}
