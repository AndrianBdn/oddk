package store_test

import (
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/store"
)

// Migration 018's two DEFAULTs point in different directions on purpose: a
// pre-existing PLAN becomes physical (the deliberate binary-by-default switch
// for already-scheduled deployments), while pre-existing HISTORY rows become
// logical (that is simply what every pre-0.1.61 archive is). SQLite's ADD
// COLUMN backfill IS the default, so inserting rows without the column proves
// exactly what upgraded rows will report.
func TestMigration018FormatDefaults(t *testing.T) {
	st := newTestStore(t)

	st.Sqlx.MustExec(`
		INSERT INTO snapshot_plans (id, utc_hour, interval_hours, cleanup_local_days, cleanup_remote_days, created_at, updated_at)
		VALUES (1, 3, 24, 7, 14, '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z')
	`)
	plan, err := st.Snapshot.GetPlan()
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || plan.Format != "physical" {
		t.Errorf("plan inserted without format = %+v, want format physical", plan)
	}

	st.Sqlx.MustExec(`
		INSERT INTO snapshot_history (filename, created_at, size, status, local_location)
		VALUES ('snapshot-old.tar.zst', '2026-01-01T00:00:00.000000000Z', 1, 'completed', '/b/snapshot-old.tar.zst')
	`)
	records, err := st.Snapshot.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Format != "logical" {
		t.Errorf("history row inserted without format = %+v, want format logical", records)
	}

	// The CHECK must reject vocabulary drift outright.
	if _, err := st.Sqlx.Exec(`UPDATE snapshot_plans SET format = 'binary'`); err == nil {
		t.Error("snapshot_plans accepted format='binary', want CHECK violation")
	}
	if _, err := st.Sqlx.Exec(`UPDATE snapshot_history SET format = 'binary'`); err == nil {
		t.Error("snapshot_history accepted format='binary', want CHECK violation")
	}
}

// Same shape as TestMigration017UpgradePath: reopening must be a no-op and 018
// must be recorded exactly once.
func TestMigration018UpgradePath(t *testing.T) {
	st, dir := newTestStoreDir(t)
	dbPath := filepath.Join(dir, "oddk.db")

	if err := st.Snapshot.SetPlan(4, 12, 9, 30, "logical"); err != nil {
		t.Fatal(err)
	}
	if err := st.Sqlx.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.NewStore(dbPath, dir)
	if err != nil {
		t.Fatalf("reopen (migrations must be idempotent): %v", err)
	}
	defer func() { _ = st2.Sqlx.Close() }()

	var n int
	if err := st2.Sqlx.Get(&n, `SELECT COUNT(*) FROM app_migrations WHERE name = '018_snapshot_format'`); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("018_snapshot_format recorded %d times, want exactly 1", n)
	}

	plan, err := st2.Snapshot.GetPlan()
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || plan.Format != "logical" || plan.UTCHour != 4 {
		t.Errorf("plan lost across reopen: %+v", plan)
	}
}
