package store_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/rfc3339time"
	"github.com/andrianbdn/oddk/internal/store"
	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
)

// Migration 019 records WHICH instances a snapshot holds and whether each was
// captured with data — the checklist's coverage verdict depends on the
// distinction between "no list recorded" (pre-019 row: unknown, reads nil) and
// "a recorded list" (authoritative, even when empty).
func TestSnapshotInstancesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewStore(filepath.Join(dir, "oddk.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Sqlx.Close() }()

	rec := &snapshotstore.Record{
		Filename:          "snapshot-a.tar.zst",
		CreatedAt:         rfc3339time.Now(),
		Size:              1,
		Status:            "completed",
		Format:            "physical",
		InstancesWithData: 1,
		ConfigOnly:        1,
		Instances: []snapshotstore.RecordInstance{
			{Name: "app", HasData: true},
			{Name: "warehouse", HasData: false},
		},
		LocalPath: "/b/snapshot-a.tar.zst",
	}
	if err := st.Snapshot.RecordSnapshot(rec); err != nil {
		t.Fatal(err)
	}

	got, err := st.Snapshot.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Instances) != 2 ||
		got.Instances[0] != (snapshotstore.RecordInstance{Name: "app", HasData: true}) ||
		got.Instances[1] != (snapshotstore.RecordInstance{Name: "warehouse", HasData: false}) {
		t.Errorf("instances did not round-trip: %+v", got.Instances)
	}

	// A pre-019 row (no instances_json) must read back nil — unknown, not
	// "no instances". The checklist treats nil as "fall back to timestamps".
	st.Sqlx.MustExec(`
		INSERT INTO snapshot_history (filename, created_at, size, status, local_location)
		VALUES ('snapshot-legacy.tar.zst', '2026-01-01T00:00:00.000000000Z', 1, 'completed', '/b/snapshot-legacy.tar.zst')
	`)
	records, err := st.Snapshot.List()
	if err != nil {
		t.Fatal(err)
	}
	legacyIdx := -1
	for i, r := range records {
		if r.Filename == "snapshot-legacy.tar.zst" {
			legacyIdx = i
		}
	}
	if legacyIdx == -1 {
		t.Fatal("legacy row not listed")
	}
	if records[legacyIdx].Instances != nil {
		t.Errorf("pre-019 row must read Instances == nil (unknown), got %+v", records[legacyIdx].Instances)
	}

	// An EMPTY deployment's snapshot records "[]", which must read back as an
	// empty non-nil slice — known-empty, distinct from unknown.
	empty := &snapshotstore.Record{
		Filename:  "snapshot-empty.tar.zst",
		CreatedAt: rfc3339time.Now(),
		Size:      1,
		Status:    "completed",
		Format:    "physical",
		Instances: []snapshotstore.RecordInstance{},
		LocalPath: "/b/snapshot-empty.tar.zst",
	}
	if err := st.Snapshot.RecordSnapshot(empty); err != nil {
		t.Fatal(err)
	}
	gotEmpty, err := st.Snapshot.Get(empty.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotEmpty.Instances == nil || len(gotEmpty.Instances) != 0 {
		t.Errorf("empty list must round-trip as non-nil empty, got %+v", gotEmpty.Instances)
	}

	// The distinction must survive JSON serialization (GET /api/snapshots,
	// snapshot list --json): known-empty is "instances": [], unknown (pre-019)
	// is "instances": null — omitempty would collapse the two.
	emptyJSON, err := json.Marshal(gotEmpty)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(emptyJSON), `"instances":[]`) {
		t.Errorf("known-empty list must serialize as \"instances\":[], got: %s", emptyJSON)
	}
	legacyJSON, err := json.Marshal(records[legacyIdx])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacyJSON), `"instances":null`) {
		t.Errorf("pre-019 row must serialize as \"instances\":null, got: %s", legacyJSON)
	}
}
