package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"github.com/andrianbdn/oddk/internal/rfc3339time"
	"github.com/andrianbdn/oddk/internal/store"
)

// retentionFixture opens a migrated store plus a second raw handle used to
// backdate rows — the store's own insert paths always stamp "now", so there is
// no other way to plant an aged record.
func retentionFixture(t *testing.T) (*store.Store, *sqlx.DB) {
	t.Helper()

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "oddk.db")

	st, err := store.NewStore(dbPath, dataDir)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := sqlx.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	return st, raw
}

func countRows(t *testing.T, raw *sqlx.DB, table string) int {
	t.Helper()
	var n int
	// #nosec G202 -- table is a test-local literal, not user input.
	if err := raw.Get(&n, "SELECT COUNT(*) FROM "+table); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestCleanupOldLogs_RetentionBoundary checks each log table's cleanup against
// the retention window.
//
// The `boundary` case is the one that pins the timestamp format, and it is
// subtle. Cleanup compares TEXT columns, so ordering is lexicographic. A
// variable-width layout like time.RFC3339Nano strips trailing zeros, rendering
// a whole-second stamp as "...T12:00:00Z" and a fractional one as
// "...T12:00:00.5Z". Since '.' (0x2E) sorts before 'Z' (0x5A), the *older*
// whole-second value then compares as GREATER and survives forever.
//
// That only bites when both values are equal down to the second — any coarser
// difference resolves at an earlier digit — so the day-scale rows below would
// NOT catch a format regression. `boundary` lands in the same second as the
// cutoff with zero nanoseconds, which is exactly the shape that breaks.
// rfc3339time pins 9 fractional digits to prevent it (see rfc3339ns).
func TestCleanupOldLogs_RetentionBoundary(t *testing.T) {
	const retention = 30 * 24 * time.Hour

	now := time.Now()
	aged := now.Add(-60 * 24 * time.Hour).Truncate(time.Second)
	agedFractional := now.Add(-31 * 24 * time.Hour)
	fresh := now.Add(-29 * 24 * time.Hour).Truncate(time.Second)
	freshFractional := now.Add(-1 * time.Hour)

	// Start of the second containing the cutoff. Cleanup derives its own cutoff
	// from a later time.Now(), so this is always strictly older and must be
	// deleted — but it is only one fractional field away, so a variable-width
	// layout would wrongly keep it.
	boundary := now.Add(-retention).Truncate(time.Second)

	stamps := []struct {
		at       time.Time
		survives bool
	}{
		{aged, false},
		{agedFractional, false},
		{boundary, false},
		{fresh, true},
		{freshFractional, true},
	}

	tests := []struct {
		table  string
		insert func(raw *sqlx.DB, at rfc3339time.Time) error
		clean  func(st *store.Store) (int64, error)
	}{
		{
			table: "cron_logs",
			insert: func(raw *sqlx.DB, at rfc3339time.Time) error {
				_, err := raw.Exec(
					`INSERT INTO cron_logs (instance_name, started_at) VALUES (?, ?)`, "app", at)
				return err
			},
			clean: func(st *store.Store) (int64, error) { return st.Cron.CleanupOldLogs(retention) },
		},
		{
			table: "notification_logs",
			insert: func(raw *sqlx.DB, at rfc3339time.Time) error {
				_, err := raw.Exec(
					`INSERT INTO notification_logs (notification_name, status, created_at) VALUES (?, ?, ?)`,
					"alerts", "sent", at)
				return err
			},
			clean: func(st *store.Store) (int64, error) {
				return st.Notifications.CleanupOldLogs(retention)
			},
		},
		{
			table: "offsite_logs",
			insert: func(raw *sqlx.DB, at rfc3339time.Time) error {
				_, err := raw.Exec(
					`INSERT INTO offsite_logs (event, offsite_settings_id, object, success, created_at)
					 VALUES (?, ?, ?, ?, ?)`, "upload", 1, "backup.tar.zst", 1, at)
				return err
			},
			clean: func(st *store.Store) (int64, error) { return st.Offsite.CleanupOldLogs(retention) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.table, func(t *testing.T) {
			st, raw := retentionFixture(t)

			wantSurvivors := 0
			for _, s := range stamps {
				if err := tc.insert(raw, rfc3339time.Time{Time: s.at}); err != nil {
					t.Fatalf("insert %s at %s: %v", tc.table, s.at, err)
				}
				if s.survives {
					wantSurvivors++
				}
			}

			deleted, err := tc.clean(st)
			if err != nil {
				t.Fatalf("cleanup %s: %v", tc.table, err)
			}

			wantDeleted := int64(len(stamps) - wantSurvivors)
			if deleted != wantDeleted {
				t.Errorf("cleanup %s deleted %d rows, want %d", tc.table, deleted, wantDeleted)
			}
			if got := countRows(t, raw, tc.table); got != wantSurvivors {
				t.Errorf("%s has %d rows after cleanup, want %d", tc.table, got, wantSurvivors)
			}
		})
	}
}

// TestCleanupOldLogs_EmptyTable guards the no-op case: sweeping an empty table
// must succeed and report nothing deleted, since the sweeper runs on every
// daemon start including the very first one.
func TestCleanupOldLogs_EmptyTable(t *testing.T) {
	st, _ := retentionFixture(t)

	cleanups := map[string]func() (int64, error){
		"cron":         func() (int64, error) { return st.Cron.CleanupOldLogs(24 * time.Hour) },
		"notification": func() (int64, error) { return st.Notifications.CleanupOldLogs(24 * time.Hour) },
		"offsite":      func() (int64, error) { return st.Offsite.CleanupOldLogs(24 * time.Hour) },
	}

	for name, clean := range cleanups {
		deleted, err := clean()
		if err != nil {
			t.Errorf("%s cleanup on empty table: %v", name, err)
		}
		if deleted != 0 {
			t.Errorf("%s cleanup deleted %d rows from empty table, want 0", name, deleted)
		}
	}
}
