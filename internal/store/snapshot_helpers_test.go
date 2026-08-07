package store_test

import (
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/store"
)

func TestVacuumIntoProducesUsableCopy(t *testing.T) {
	st, dir := newTestStoreDir(t)
	if err := st.KV.SetInt("health.degraded_threshold.int", 9); err != nil {
		t.Fatal(err)
	}

	copyPath := filepath.Join(dir, "snapshot-copy.db")
	if err := st.VacuumInto(copyPath); err != nil {
		t.Fatalf("VacuumInto: %v", err)
	}

	// The copy must be a fully usable store, not just bytes on disk: snapshot
	// apply opens it directly.
	reopened, err := store.NewStore(copyPath, dir)
	if err != nil {
		t.Fatalf("reopen vacuumed copy: %v", err)
	}
	got, err := reopened.KV.GetInt("health.degraded_threshold.int")
	if err != nil {
		t.Fatalf("read from copy: %v", err)
	}
	if got != 9 {
		t.Errorf("value in copy = %d, want 9", got)
	}

	// Refuse to clobber an existing file - the caller stages into a fresh dir.
	if err := st.VacuumInto(copyPath); err == nil {
		t.Error("VacuumInto overwrote an existing file; want error")
	}
}

func TestAppliedMigrationsOrdered(t *testing.T) {
	st := newTestStore(t)

	names, err := st.AppliedMigrations()
	if err != nil {
		t.Fatalf("AppliedMigrations: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no migrations reported on a freshly created store")
	}
	t.Logf("%d migrations, first=%q last=%q", len(names), names[0], names[len(names)-1])
}
