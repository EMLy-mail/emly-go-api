package models

import "time"

type FileRole string

const (
	FileRoleAttachment FileRole = "attachment"
	FileRoleScreenshot FileRole = "screenshot"
	FileRoleLog        FileRole = "log"
)

type BugReportFile struct {
	ID          int64     `db:"id"            json:"id"`
	BugReportID int64     `db:"bug_report_id" json:"bug_report_id"`
	Filename    string    `db:"filename"      json:"filename"`
	MimeType    string    `db:"mime_type"     json:"mime_type"`
	Role        FileRole  `db:"role"          json:"role"`
	Data        []byte    `db:"data"          json:"-"`
	CreatedAt   time.Time `db:"created_at"   json:"created_at"`
}
