package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/rfc3339time"
	"github.com/andrianbdn/oddk/internal/store/instances"
)

func TestInstanceStoreLifecycle(t *testing.T) {
	st := newTestStore(t)

	created, err := st.Instances.Create("alpha", 5432, "17", "enc", "", 2, 2048, "default", "postgres:17")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Name != "alpha" || created.Port != 5432 || created.CPUCores != 2 || created.RAMMB != 2048 {
		t.Fatalf("Create returned %+v", created)
	}

	if inUse, holder, err := st.Instances.IsPortInUse(5432); err != nil || !inUse || holder != "alpha" {
		t.Fatalf("IsPortInUse(5432) = %v %q %v, want true alpha", inUse, holder, err)
	}
	if inUse, _, err := st.Instances.IsPortInUse(5433); err != nil || inUse {
		t.Fatalf("IsPortInUse(5433) = %v %v, want false", inUse, err)
	}
	if used, err := st.Instances.IsNameInUse("alpha"); err != nil || !used {
		t.Fatalf("IsNameInUse(alpha) = %v %v, want true", used, err)
	}

	// Every declared status is storable; anything else is refused by the CHECK
	// constraint migration 020 built from the same list, so the schema and the
	// code cannot drift.
	for _, status := range instances.AllStatuses() {
		if err := st.Instances.UpdateStatus("alpha", status); err != nil {
			t.Errorf("UpdateStatus(%q): %v", status, err)
		}
	}
	if err := st.Instances.UpdateStatus("alpha", instances.InstanceStatus("bogus")); err == nil {
		t.Fatal("UpdateStatus accepted a status outside the declared vocabulary")
	}

	if err := st.Instances.UpdateResources("alpha", 5440, 4, 8192); err != nil {
		t.Fatalf("UpdateResources: %v", err)
	}
	got, err := st.Instances.Get("alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Port != 5440 || got.CPUCores != 4 || got.RAMMB != 8192 {
		t.Fatalf("after UpdateResources: port %d cpu %d ram %d", got.Port, got.CPUCores, got.RAMMB)
	}
	if inUse, holder, _ := st.Instances.IsPortInUse(5440); !inUse || holder != "alpha" {
		t.Fatalf("the moved port is not reported in use: %v %q", inUse, holder)
	}

	// Backdating is an unconditional write here; the "never forward-date" rule
	// belongs to the caller (restore-instance), which checks the row's age
	// first. What the store must guarantee is that the value round-trips
	// exactly, since checklist coverage compares it against snapshot times.
	older := rfc3339time.Time{Time: time.Now().Add(-72 * time.Hour).UTC()}
	if err := st.Instances.BackdateCreatedAt("alpha", older); err != nil {
		t.Fatalf("BackdateCreatedAt: %v", err)
	}
	got, _ = st.Instances.Get("alpha")
	if !got.CreatedAt.Equal(older.Time) {
		t.Fatalf("CreatedAt after backdate = %v, want %v", got.CreatedAt, older.Time)
	}

	if err := st.Instances.Delete("alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Instances.Get("alpha"); !errors.Is(err, operr.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want operr.ErrNotFound", err)
	}
	if err := st.Instances.UpdateResources("alpha", 1, 1, 128); err == nil {
		t.Fatal("UpdateResources on a deleted instance should fail")
	}
}
