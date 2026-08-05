package operations

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrianbdn/oddk/internal/rfc3339time"
	"github.com/andrianbdn/oddk/internal/store"
	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
)

// The coverage verdict is the audit's core claim, so every state transition is
// pinned here: config-only must never read as covered, a younger instance (or
// a same-name re-creation) must never inherit its predecessor's coverage, and
// only a pre-019 record may fall back to optimistic timestamp coverage.
func TestSnapshotCoverage(t *testing.T) {
	capture := time.Date(2026, 7, 8, 2, 30, 0, 0, time.UTC)
	ref := &ChecklistArchive{ID: 7}
	rec := func(instances []snapshotstore.RecordInstance) *snapshotstore.Record {
		return &snapshotstore.Record{
			ID:        7,
			CreatedAt: rfc3339time.Time{Time: capture},
			Instances: instances,
		}
	}
	before := capture.Add(-time.Hour)
	after := capture.Add(time.Hour)

	tests := []struct {
		name      string
		newest    *snapshotstore.Record
		createdAt time.Time
		wantState string
		wantRef   bool
	}{
		{"no snapshots at all", nil, before, "no-snapshots", false},
		{
			"captured with data",
			rec([]snapshotstore.RecordInstance{{Name: "app", HasData: true}}),
			before, "covered", true,
		},
		{
			"captured configuration-only",
			rec([]snapshotstore.RecordInstance{{Name: "app", HasData: false}}),
			before, "config-only", true,
		},
		{
			"created after the capture",
			rec([]snapshotstore.RecordInstance{{Name: "app", HasData: true}}),
			after, "not-captured", false,
		},
		{
			// Destroyed and re-created under the same name after the capture:
			// the archive's "app" entry holds the predecessor's data.
			"same-name re-creation after capture",
			rec([]snapshotstore.RecordInstance{{Name: "app", HasData: true}}),
			after, "not-captured", false,
		},
		{
			"existed at capture but absent from the entry list",
			rec([]snapshotstore.RecordInstance{{Name: "other", HasData: true}}),
			before, "not-captured", false,
		},
		{
			// Pre-019 record: data-presence unknowable, optimistic by design.
			"pre-019 record falls back to timestamps",
			rec(nil),
			before, "covered", true,
		},
		{
			"pre-019 record still respects creation time",
			rec(nil),
			after, "not-captured", false,
		},
		{
			// Restored instance backdated to the capture time: equal, not
			// before, must count as existing at capture.
			"created exactly at the capture time",
			rec([]snapshotstore.RecordInstance{{Name: "app", HasData: true}}),
			capture, "covered", true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := snapshotCoverage(tt.newest, ref, "app", tt.createdAt)
			if got.State != tt.wantState {
				t.Errorf("state = %q, want %q", got.State, tt.wantState)
			}
			if (got.Snapshot != nil) != tt.wantRef {
				t.Errorf("snapshot ref present = %v, want %v", got.Snapshot != nil, tt.wantRef)
			}
		})
	}
}

// A completed record whose archive exists nowhere (local file deleted outside
// ODDK, no remote copy) must not be selected as the newest snapshot: a
// "covered" verdict or a green last-snapshot line naming an archive with zero
// copies would describe a restore that cannot happen.
func TestCollectSnapshotsSkipsCopylessNewest(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewStore(filepath.Join(dir, "oddk.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Sqlx.Close() }()

	// Older snapshot: file actually on disk.
	surviving := filepath.Join(dir, "snapshot-old.tar.zst")
	if err := os.WriteFile(surviving, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	older := &snapshotstore.Record{
		Filename:  "snapshot-old.tar.zst",
		CreatedAt: rfc3339time.Time{Time: time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC)},
		Size:      1,
		Status:    "completed",
		Format:    "physical",
		Instances: []snapshotstore.RecordInstance{},
		LocalPath: surviving,
	}
	if err := st.Snapshot.RecordSnapshot(older); err != nil {
		t.Fatal(err)
	}

	// Newer snapshot: catalogued local-only, but the file was deleted outside
	// ODDK and there is no remote copy.
	gone := &snapshotstore.Record{
		Filename:  "snapshot-new.tar.zst",
		CreatedAt: rfc3339time.Time{Time: time.Date(2026, 7, 8, 3, 0, 0, 0, time.UTC)},
		Size:      1,
		Status:    "completed",
		Format:    "physical",
		Instances: []snapshotstore.RecordInstance{},
		LocalPath: filepath.Join(dir, "snapshot-new.tar.zst"), // never written
	}
	if err := st.Snapshot.RecordSnapshot(gone); err != nil {
		t.Fatal(err)
	}

	op := NewChecklistOp(&Dependencies{Store: st})
	var out ChecklistSnapshots
	newest, err := op.collectSnapshots(&out)
	if err != nil {
		t.Fatal(err)
	}

	if newest == nil || newest.ID != older.ID {
		t.Fatalf("newest = %+v, want the older record with a surviving copy (id %d)", newest, older.ID)
	}
	if out.LastSnapshot == nil || out.LastSnapshot.ID != older.ID {
		t.Errorf("LastSnapshot = %+v, want id %d", out.LastSnapshot, older.ID)
	}
	// The copy-less record must still be visible as an orphan in the buckets.
	if out.Copies.None != 1 || out.Copies.LocalOnly != 1 {
		t.Errorf("copies = %+v, want 1 local-only (surviving) and 1 with no copies", out.Copies)
	}
}
