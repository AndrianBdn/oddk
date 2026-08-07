package store_test

import (
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/store"
)

// Simulates an existing deployment upgrading: open the store (applies 001-017),
// close, reopen (must be a no-op), and confirm 017 is recorded exactly once.
func TestMigration017UpgradePath(t *testing.T) {
	st, dir := newTestStoreDir(t)
	dbPath := filepath.Join(dir, "oddk.db")

	// Pre-existing data must survive the new migration.
	if _, err := st.Instances.Create("legacy", 5432, "17", "enc", "", 1, 512, "default", "postgres:17"); err != nil {
		t.Fatal(err)
	}
	if err := st.Cron.CreatePlan("legacy", 3, 7, 14); err != nil {
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
	if err := st2.Sqlx.Get(&n, `SELECT COUNT(*) FROM app_migrations WHERE name = '017_snapshot_tables'`); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("017_snapshot_tables recorded %d times, want exactly 1", n)
	}

	inst, err := st2.Instances.Get("legacy")
	if err != nil || inst == nil {
		t.Fatalf("pre-existing instance lost across the upgrade: (%v, %v)", inst, err)
	}
	plan, err := st2.Cron.GetPlan("legacy")
	if err != nil || plan == nil {
		t.Fatalf("pre-existing cron plan lost across the upgrade: (%v, %v)", plan, err)
	}
	// The new tables must exist and be empty, not pre-populated.
	if err := st2.Sqlx.Get(&n, `SELECT COUNT(*) FROM snapshot_plans`); err != nil {
		t.Fatalf("snapshot_plans missing after upgrade: %v", err)
	}
	if n != 0 {
		t.Errorf("snapshot_plans has %d rows on a freshly-migrated store, want 0", n)
	}
	if err := st2.Sqlx.Get(&n, `SELECT COUNT(*) FROM snapshot_history`); err != nil {
		t.Fatalf("snapshot_history missing after upgrade: %v", err)
	}
}
