package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// NullJSON is a JSON column that may be SQL NULL. json.RawMessage cannot be
// used directly: it does not implement sql.Scanner, and database/sql has no
// way to store a NULL into it, so a single NULL row fails the whole query.
// NULL scans to an empty value, which `omitempty` drops from the response.
type NullJSON json.RawMessage

func (j *NullJSON) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		// The driver reuses its buffer on the next row, so copy.
		*j = append(NullJSON(nil), v...)
	case string:
		*j = NullJSON(v)
	default:
		return fmt.Errorf("models.NullJSON: cannot scan %T", src)
	}
	return nil
}

func (j NullJSON) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	return []byte(j), nil
}

func (j NullJSON) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}
