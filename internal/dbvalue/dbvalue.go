// Package dbvalue converts between Go values and the shape the database
// columns behind them expect: the empty string and the zero time become SQL
// NULL, a NULL pointer reads back as an empty string, and an over-long string
// is clamped to what its column can hold.
//
// These live together in one leaf package because five feature packages need
// them (bans, updates, updaterclient, clientws, stats) and none of them owns
// the rule. "Absent means NULL" in particular is a decision the whole schema
// depends on - see the upsert conventions in internal/updaterclient, where a
// header the client did not send must not overwrite what earlier sightings
// established.
package dbvalue

import "time"

// NullString returns nil for the empty string, so an absent value is stored as
// SQL NULL rather than as an empty column.
func NullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// NullTime returns nil for the zero time, the time equivalent of NullString.
func NullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// Deref reads a nullable column back as a plain string, with NULL becoming "".
// The inverse of NullString, for log lines and payloads that would rather carry
// an empty string than a nil pointer.
func Deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Truncate clamps s to max characters, counted in runes rather than bytes so a
// column sized in characters is never overrun by multi-byte input and a
// multi-byte account name is never cut mid-character.
//
// The values it guards are free-form strings from firmware, from Windows
// account names and from an app's own version string, with no protocol bound on
// their length. An over-long one would fail the whole upsert and cost that
// client its telemetry row, which is a poor trade for a value nobody reads past
// the first few dozen characters.
func Truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
