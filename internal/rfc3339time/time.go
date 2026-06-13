package rfc3339time

import (
	"database/sql/driver"
	"fmt"
	"math"
	"time"
)

// Time wraps time.Time to handle RFC3339 serialization for SQLite TEXT columns
type Time struct{ time.Time }

// Fixed 9-digit fractional seconds so TEXT sorts correctly lexicographically
const rfc3339ns = "2006-01-02T15:04:05.000000000Z07:00"

// Now returns the current time as rfc3339time.Time
func Now() Time {
	return Time{time.Now()}
}

func Parse(layout, value string) (Time, error) {
	t, err := time.Parse(layout, value)
	return Time{t}, err
}

// Value implements driver.Valuer for database storage
func (t Time) Value() (driver.Value, error) {
	if t.IsZero() {
		return nil, nil // store NULL for zero value
	}
	return t.UTC().Format(rfc3339ns), nil // stored as TEXT (RFC 3339, UTC)
}

// Scan implements sql.Scanner for database retrieval
func (t *Time) Scan(src any) error {
	switch v := src.(type) {
	case time.Time:
		t.Time = v
		return nil
	case []byte:
		return t.parse(string(v))
	case string:
		return t.parse(v)
	case int64: // unix seconds (if you ever read INTEGER timestamps)
		t.Time = time.Unix(v, 0).UTC()
		return nil
	case float64: // unix seconds with fraction
		sec, frac := math.Modf(v)
		t.Time = time.Unix(int64(sec), int64(frac*1e9)).UTC()
		return nil
	case nil:
		t.Time = time.Time{}
		return nil
	default:
		return fmt.Errorf("unsupported time type %T", src)
	}
}

func (t *Time) parse(s string) error {
	// try our fixed layout first, then the standard variants commonly seen
	if tt, err := time.Parse(rfc3339ns, s); err == nil {
		t.Time = tt
		return nil
	}
	if tt, err := time.Parse(time.RFC3339Nano, s); err == nil {
		t.Time = tt
		return nil
	}
	if tt, err := time.Parse(time.RFC3339, s); err == nil {
		t.Time = tt
		return nil
	}
	// common SQLite text format without TZ
	if tt, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		t.Time = tt.UTC()
		return nil
	}
	return fmt.Errorf("cannot parse time %q", s)
}

// MarshalJSON implements json.Marshaler
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return fmt.Appendf(nil, "%q", t.Format(time.RFC3339)), nil
}

// UnmarshalJSON implements json.Unmarshaler
func (t *Time) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		t.Time = time.Time{}
		return nil
	}
	if len(data) >= 2 && data[0] == '"' && data[len(data)-1] == '"' {
		data = data[1 : len(data)-1]
	}
	return t.parse(string(data))
}

// String returns the time formatted as RFC3339
func (t Time) String() string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
