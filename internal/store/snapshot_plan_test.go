package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/rfc3339time"
	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
)

// TestPlanRunsAtHour pins the anchored-interval schedule.
//
// The anchor is what makes a sub-daily interval predictable: "every 6 hours"
// alone does not say WHICH hours. It also has to hold across midnight, which is
// why intervals are restricted to divisors of 24 — an interval of 5 anchored at
// 03 would fire 03,08,13,18,23 and then 03 again, a 4-hour gap.
func TestPlanRunsAtHour(t *testing.T) {
	tests := []struct {
		name     string
		anchor   int
		interval int
		want     []int
	}{
		{"daily at 03", 3, 24, []int{3}},
		{"daily at 00", 0, 24, []int{0}},
		{"every 6h from 03", 3, 6, []int{3, 9, 15, 21}},
		{"every 12h from 22 wraps midnight", 22, 12, []int{10, 22}},
		{"every 8h from 05", 5, 8, []int{5, 13, 21}},
		{"hourly", 0, 1, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := &snapshotstore.Plan{UTCHour: tc.anchor, IntervalHours: tc.interval}
			want := make(map[int]bool, len(tc.want))
			for _, h := range tc.want {
				want[h] = true
			}
			for h := range 24 {
				if got := plan.RunsAtHour(h); got != want[h] {
					t.Errorf("hour %02d: RunsAtHour = %v, want %v", h, got, want[h])
				}
			}
		})
	}
}

// TestPlanRunsAtHourZeroInterval guards the defensive branch: a zero interval
// must not divide by zero, which would panic inside the scheduler goroutine and
// take the whole cron loop down.
func TestPlanRunsAtHourZeroInterval(t *testing.T) {
	plan := &snapshotstore.Plan{UTCHour: 7, IntervalHours: 0}
	if !plan.RunsAtHour(7) {
		t.Error("a zero interval should fall back to daily at the anchor hour")
	}
	if plan.RunsAtHour(8) {
		t.Error("a zero interval should not fire at every hour")
	}
}

func TestSnapshotPlanIsSingleton(t *testing.T) {
	st := newTestStore(t)

	if plan, err := st.Snapshot.GetPlan(); err != nil || plan != nil {
		t.Fatalf("GetPlan on a fresh store = (%v, %v), want (nil, nil)", plan, err)
	}

	if err := st.Snapshot.SetPlan(3, 24, 7, 14, "physical"); err != nil {
		t.Fatal(err)
	}
	// A second SetPlan must REPLACE, not add: the scheduler reads one row, and a
	// second would make which schedule applies a coin flip.
	if err := st.Snapshot.SetPlan(9, 6, 3, 5, "logical"); err != nil {
		t.Fatal(err)
	}

	plan, err := st.Snapshot.GetPlan()
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || plan.UTCHour != 9 || plan.IntervalHours != 6 ||
		plan.CleanupLocalDays != 3 || plan.CleanupRemoteDays != 5 || plan.Format != "logical" {
		t.Errorf("plan = %+v, want the second SetPlan's values", plan)
	}

	var count int
	if err := st.Sqlx.Get(&count, `SELECT COUNT(*) FROM snapshot_plans`); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("snapshot_plans holds %d rows, want exactly 1", count)
	}

	if err := st.Snapshot.DeletePlan(); err != nil {
		t.Fatal(err)
	}
	if plan, err := st.Snapshot.GetPlan(); err != nil || plan != nil {
		t.Errorf("after DeletePlan, GetPlan = (%v, %v), want (nil, nil)", plan, err)
	}
}

// TestSnapshotRecordLocationInvariant covers the CHECK that a record must
// describe at least one copy.
//
// Clearing the last location has to be refused in Go rather than left to the
// constraint: the same mistake in the backup catalogue produced a constraint
// error in the middle of a destructive apply, which is far worse than a refusal.
func TestSnapshotRecordLocationInvariant(t *testing.T) {
	st := newTestStore(t)

	rec := &snapshotstore.Record{
		Filename:  "snapshot-host-20260730120000.tar.zst",
		CreatedAt: rfc3339time.Now(),
		Size:      1234,
		Status:    "completed",
		LocalPath: "/var/lib/oddk/backups/snapshot-host-20260730120000.tar.zst",
	}
	if err := st.Snapshot.RecordSnapshot(rec); err != nil {
		t.Fatal(err)
	}
	if rec.ID == 0 {
		t.Fatal("RecordSnapshot did not set the record ID")
	}

	// Local-only: clearing local would leave nothing, so it must refuse.
	if err := st.Snapshot.ClearLocalLocation(rec.ID); err == nil {
		t.Error("clearing the only location was allowed; it must refuse so the caller deletes the record instead")
	}

	if err := st.Snapshot.SetRemoteLocation(rec.ID, "s3://bucket/*snapshots*/2026-07-30/snap.tar.zst"); err != nil {
		t.Fatal(err)
	}
	// With a remote copy present, clearing local is now legitimate.
	if err := st.Snapshot.ClearLocalLocation(rec.ID); err != nil {
		t.Errorf("clearing local with a remote copy present: %v", err)
	}
	// And now clearing remote must refuse, for the same reason.
	if err := st.Snapshot.ClearRemoteLocation(rec.ID); err == nil {
		t.Error("clearing the last remaining location was allowed")
	}

	got, err := st.Snapshot.Get(rec.ID)
	if err != nil || got == nil {
		t.Fatalf("Get = (%v, %v)", got, err)
	}
	if got.LocalPath != "" {
		t.Errorf("local path = %q, want empty after clearing", got.LocalPath)
	}
	if got.RemotePath == "" {
		t.Error("remote path was lost")
	}
}

// TestSnapshotReconcileLocalLocations covers the post-`snapshot apply` fixup.
//
// A restored oddk.db carries the SOURCE host's absolute paths and the archive
// contains no other snapshots, so without this every record claims a local copy
// that is not on this machine. This mirrors the backup-catalogue reconciliation
// and inherits the same constraint: a record with no file AND no remote copy
// cannot have its last location cleared, so it is counted, not mutated.
func TestSnapshotReconcileLocalLocations(t *testing.T) {
	st := newTestStore(t)
	backupDir := t.TempDir()

	presentName := "snapshot-oldhost-20260101000000.tar.zst"
	if err := os.WriteFile(filepath.Join(backupDir, presentName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Copied across by the operator: must be re-pointed, not lost.
	present := &snapshotstore.Record{
		Filename: presentName, CreatedAt: rfc3339time.Now(), Status: "completed",
		LocalPath: "/srv/oldhost/backups/" + presentName,
	}
	// Absent here but offsite: local cleared, record survives.
	withRemote := &snapshotstore.Record{
		Filename: "snapshot-oldhost-20260101000001.tar.zst", CreatedAt: rfc3339time.Now(), Status: "completed",
		LocalPath: "/srv/oldhost/backups/snapshot-oldhost-20260101000001.tar.zst",
	}
	// Absent here, no offsite: nothing can recover it, so it must be COUNTED.
	dangling := &snapshotstore.Record{
		Filename: "snapshot-oldhost-20260101000002.tar.zst", CreatedAt: rfc3339time.Now(), Status: "completed",
		LocalPath: "/srv/oldhost/backups/snapshot-oldhost-20260101000002.tar.zst",
	}
	for _, r := range []*snapshotstore.Record{present, withRemote, dangling} {
		if err := st.Snapshot.RecordSnapshot(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Snapshot.SetRemoteLocation(withRemote.ID, "s3://bucket/x.tar.zst"); err != nil {
		t.Fatal(err)
	}

	repointed, cleared, danglingAfter, err := st.Snapshot.ReconcileLocalLocations(backupDir)
	if err != nil {
		t.Fatalf("ReconcileLocalLocations: %v", err)
	}
	if repointed != 1 {
		t.Errorf("repointed = %d, want 1", repointed)
	}
	if cleared != 1 {
		t.Errorf("cleared = %d, want 1", cleared)
	}
	if danglingAfter != 1 {
		t.Errorf("dangling = %d, want 1", danglingAfter)
	}

	got, err := st.Snapshot.Get(present.ID)
	if err != nil || got == nil {
		t.Fatalf("Get = (%v, %v)", got, err)
	}
	want := filepath.Join(backupDir, presentName)
	if got.LocalPath != want {
		t.Errorf("re-pointed local path = %q, want %q", got.LocalPath, want)
	}

	// The sweep must be able to tell a managed snapshot from a genuine orphan.
	referenced, err := st.Snapshot.ReferencedFilenames()
	if err != nil {
		t.Fatal(err)
	}
	if !referenced[presentName] {
		t.Error("ReferencedFilenames does not include a snapshot the catalogue points at")
	}
	if referenced["snapshot-unmanaged-20260101000003.tar.zst"] {
		t.Error("ReferencedFilenames claims an unmanaged archive is referenced")
	}
}

// TestSnapshotReconcileIgnoresDownloadsDir pins a contract that used to be
// implicit: ReconcileLocalLocations resolves records against the backup dir's
// TOP LEVEL only. The managed downloads area (backupDir/downloads/) holds
// deliberately uncatalogued archives, and a reconcile that descended into it
// would re-point catalogue rows at files whose lifecycle is a 7-day TTL sweep
// — turning managed local copies into ones that silently evaporate.
func TestSnapshotReconcileIgnoresDownloadsDir(t *testing.T) {
	st := newTestStore(t)
	backupDir := t.TempDir()

	name := "snapshot-oldhost-20260201000000.tar.zst"
	if err := os.MkdirAll(filepath.Join(backupDir, "downloads"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "downloads", name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := &snapshotstore.Record{
		Filename: name, CreatedAt: rfc3339time.Now(), Status: "completed",
		LocalPath: "/srv/oldhost/backups/" + name,
	}
	if err := st.Snapshot.RecordSnapshot(rec); err != nil {
		t.Fatal(err)
	}
	if err := st.Snapshot.SetRemoteLocation(rec.ID, "s3://bucket/"+name); err != nil {
		t.Fatal(err)
	}

	repointed, cleared, _, err := st.Snapshot.ReconcileLocalLocations(backupDir)
	if err != nil {
		t.Fatalf("ReconcileLocalLocations: %v", err)
	}
	if repointed != 0 || cleared != 1 {
		t.Errorf("repointed/cleared = %d/%d, want 0/1 (file exists only under downloads/)", repointed, cleared)
	}
}

// TestSnapshotFindByRemoteLocation pins the restore-by-URI shortcut: a URI
// that IS a catalogue row's remote copy resolves to that row, so the archive
// is materialized as the row's managed local copy instead of a second,
// unmanaged download.
func TestSnapshotFindByRemoteLocation(t *testing.T) {
	st := newTestStore(t)

	rec := &snapshotstore.Record{
		Filename: "snapshot-host-20260301000000.tar.zst", CreatedAt: rfc3339time.Now(), Status: "completed",
		LocalPath: "/backups/snapshot-host-20260301000000.tar.zst",
	}
	if err := st.Snapshot.RecordSnapshot(rec); err != nil {
		t.Fatal(err)
	}
	loc := "s3://bucket/oddk/*snapshots*/2026-03-01/snapshot-host-20260301000000.tar.zst"
	if err := st.Snapshot.SetRemoteLocation(rec.ID, loc); err != nil {
		t.Fatal(err)
	}

	got, err := st.Snapshot.FindByRemoteLocation(loc)
	if err != nil {
		t.Fatalf("FindByRemoteLocation: %v", err)
	}
	if got == nil || got.ID != rec.ID {
		t.Fatalf("FindByRemoteLocation = %+v, want record %d", got, rec.ID)
	}

	miss, err := st.Snapshot.FindByRemoteLocation("s3://bucket/nothing.tar.zst")
	if err != nil || miss != nil {
		t.Errorf("miss = (%v, %v), want (nil, nil)", miss, err)
	}
}
