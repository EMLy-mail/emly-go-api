package obfuscate

import "time"

// DateLayout is the layout used for the "shift_date" string exchanged between
// server and client. Both sides derive the shift from this STRING, never from
// their own clock, so time-zone differences cannot misalign them.
const DateLayout = "2006-01-02"

// ShiftFromDateString turns a "YYYY-MM-DD" date into the daily cipher shift:
//   - even month  -> positive shift (the cipher advances)
//   - odd month    -> negative shift (the cipher retreats)
//   - |shift| = day of the month
func ShiftFromDateString(dateStr string) (int, error) {
	t, err := time.Parse(DateLayout, dateStr)
	if err != nil {
		return 0, err
	}
	if int(t.Month())%2 == 0 {
		return t.Day(), nil
	}
	return -t.Day(), nil
}
