package rfc3339time_test

import (
	"testing"
	"time"

	"github.com/andrianbdn/oddk/internal/rfc3339time"
)

// Rows written before CronStore.UpdateLog normalized its values carry Go's
// default time.Time.String() layout in a TEXT column that this scanner expects
// to be RFC3339. They must still read: sqlx fails the ENTIRE SELECT on one
// unparseable column, so a single legacy row would make `oddk cron logs`
// useless on precisely the long-lived deployments whose history matters most.
func TestScan_GoDefaultStringLayout(t *testing.T) {
	// Exactly what was found in cron_logs.backup_finished_at in the wild.
	const legacy = "2026-08-11 04:53:54.122388675 +0000 UTC"

	var got rfc3339time.Time
	if err := got.Scan(legacy); err != nil {
		t.Fatalf("Scan(%q) failed: %v", legacy, err)
	}

	want := time.Date(2026, 8, 11, 4, 53, 54, 122388675, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Scan(%q) = %v, want %v", legacy, got.Time, want)
	}
	if got.Location() != time.UTC {
		t.Errorf("parsed time should be normalized to UTC, got %v", got.Location())
	}
}

// A value that round-trips through Value() must come back identical, so the
// legacy fallback above cannot mask a regression in the canonical path.
func TestValueScanRoundTrip(t *testing.T) {
	original := rfc3339time.Time{Time: time.Date(2026, 8, 11, 4, 53, 54, 122388675, time.UTC)}

	v, err := original.Value()
	if err != nil {
		t.Fatalf("Value() failed: %v", err)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("Value() returned %T, want string (the column is TEXT)", v)
	}

	var back rfc3339time.Time
	if err := back.Scan(s); err != nil {
		t.Fatalf("Scan(%q) failed: %v", s, err)
	}
	if !back.Equal(original.Time) {
		t.Errorf("round trip changed the value: %v -> %q -> %v", original.Time, s, back.Time)
	}
}

func TestScan_StillRejectsGarbage(t *testing.T) {
	var got rfc3339time.Time
	if err := got.Scan("not a timestamp at all"); err == nil {
		t.Error("expected an error for unparseable input; a silent zero time would be worse than a failure")
	}
}
