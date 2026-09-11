package models

import "time"

// BanType is which identifier a Ban matches on. The three are independent:
// banning a machine by HWID says nothing about its IP, and a machine that
// must be cut off whatever it renames itself to needs a hwid ban, not a
// hostname one.
type BanType string

const (
	// BanTypeIP matches the peer address the request arrived from, exactly.
	BanTypeIP BanType = "ip"
	// BanTypeHWID matches X-EMLy-HWID. The strongest of the three: it
	// survives a rename, a re-IP and an AD domain change.
	BanTypeHWID BanType = "hwid"
	// BanTypeHostname matches X-EMLy-Hostname, case-insensitively.
	BanTypeHostname BanType = "hostname"
)

// ValidBanType reports whether t is one of the three known types. Used to
// reject anything else at the handler rather than storing a row no matcher
// will ever look at.
func ValidBanType(t BanType) bool {
	switch t {
	case BanTypeIP, BanTypeHWID, BanTypeHostname:
		return true
	}
	return false
}

// Ban is one permanent block. There is deliberately no expiry column: these
// are the bans an operator sets by hand and removes by hand, distinct from
// the automatic, time-boxed ones the rate limiter keeps in memory.
type Ban struct {
	ID        int64     `db:"id"         json:"id"`
	BanType   BanType   `db:"ban_type"   json:"ban_type"`
	Value     string    `db:"value"      json:"value"`
	Reason    *string   `db:"reason"     json:"reason,omitempty"`
	CreatedBy *string   `db:"created_by" json:"created_by,omitempty"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}
