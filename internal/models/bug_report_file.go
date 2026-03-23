package models

import "time"

type FileRole string

const (
	FileRoleScreenshot   FileRole = "screenshot"
	FileRoleMailFile     FileRole = "mail_file"
	FileRoleLocalStorage FileRole = "localstorage"
	FileRoleConfig       FileRole = "config"
)

type BugReportFile struct {
	ID          int64     `db:"id"            json:"id"`
	BugReportID int64     `db:"report_id" json:"report_id"`
	FileRole    FileRole  `db:"file_role"          json:"file_role"`
	Filename    string    `db:"filename"      json:"filename"`
	MimeType    string    `db:"mime_type"     json:"mime_type"`
	FileSize    int64     `db:"file_size"    json:"file_size"`
	Data        []byte    `db:"data"          json:"-"`
	CreatedAt   time.Time `db:"created_at"   json:"created_at"`
}
