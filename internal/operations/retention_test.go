package operations

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
)

// The regression this whole file exists for: the floor used to count catalogue
// ROWS. Two rows whose archives had been deleted out of band consumed both
// slots, and retention then deleted the newest archive that actually existed —
// the precise loss the floor was added to prevent, caused by the floor.
func TestRetentionFloor_GhostRecordsDoNotConsumeSlots(t *testing.T) {
	floor := newRetentionFloor(2)

	// Newest-first, as every List/ListBackups returns: two records whose files
	// are gone, then three real archives.
	present := []bool{false, false, true, true, true}
	var protected []int
	for i, p := range present {
		if floor.protects(p) {
			protected = append(protected, i)
		}
	}

	if len(protected) != 2 || protected[0] != 2 || protected[1] != 3 {
		t.Fatalf("floor protected records %v, want the two newest SURVIVING ones (2 and 3)", protected)
	}
	if !floor.full() {
		t.Error("floor reports itself unfilled after protecting its quota")
	}
}

func TestSnapshotRetentionProtects_DegradedDoNotEvictLastComplete(t *testing.T) {
	// Newest-first: two degraded surviving copies, then one complete, then another complete.
	// newest-2 keeps the degraded pair; the complete slot keeps the first complete
	// (index 2); the older complete (index 3) is eligible for age-based delete.
	type rec struct {
		present  bool
		complete bool
	}
	records := []rec{
		{present: true, complete: false},
		{present: true, complete: false},
		{present: true, complete: true},
		{present: true, complete: true},
	}
	floor := newRetentionFloor(2)
	completeKept := false
	var kept []int
	for i, r := range records {
		if snapshotRetentionProtects(floor, &completeKept, r.present, r.complete, true).keep != retentionKeepNone {
			kept = append(kept, i)
		}
	}
	if len(kept) != 3 || kept[0] != 0 || kept[1] != 1 || kept[2] != 2 {
		t.Fatalf("kept %v, want the two newest copies plus the newest complete (0,1,2)", kept)
	}
}

func TestSnapshotRetentionProtects_CompleteNewestFillsBothRules(t *testing.T) {
	floor := newRetentionFloor(2)
	completeKept := false
	if got := snapshotRetentionProtects(floor, &completeKept, true, true, true); got.keep != retentionKeepNewest {
		t.Fatalf("newest complete copy is kept by newest-N first, got %v", got.keep)
	}
	if !completeKept {
		t.Fatal("complete slot should be filled by the newest complete copy")
	}
	if got := snapshotRetentionProtects(floor, &completeKept, true, true, true); got.keep != retentionKeepNewest {
		t.Fatalf("second-newest complete still fills newest-N, got %v", got.keep)
	}
	if got := snapshotRetentionProtects(floor, &completeKept, true, true, true); got.keep != retentionKeepNone {
		t.Fatalf("third copy should not be protected by either rule, got %v", got.keep)
	}
}

// The defect this bounds: the complete-archive pin had no age limit at all. A
// deployment with a permanently configuration-only instance never produces
// another complete archive, so the last one was held forever — on the disk and
// in the bucket — while the checklist reported the instance as uncovered.
func TestSnapshotRetentionProtects_PinReleasesOnceGraceExpires(t *testing.T) {
	// Newest-first: two degraded copies fill newest-2, then the newest complete
	// archive, now older than the pin window.
	floor := newRetentionFloor(2)
	completeKept := false

	for i := range 2 {
		if got := snapshotRetentionProtects(floor, &completeKept, true, false, true); got.keep != retentionKeepNewest {
			t.Fatalf("degraded copy %d should fill newest-N, got %v", i, got.keep)
		}
	}

	got := snapshotRetentionProtects(floor, &completeKept, true, true, false)
	if got.keep != retentionKeepNone {
		t.Fatalf("a complete archive past the pin window must not be kept, got %v", got.keep)
	}
	if !got.pinExpired {
		t.Fatal("releasing the last complete archive must be reported, not done silently")
	}
	if !completeKept {
		t.Fatal("an expired pin still consumes the complete slot: every archive below it is older still")
	}
}

// pinExpired says "the last complete archive is going". It must never be set for
// a copy that is in fact surviving, or the warning would fire on a healthy run.
func TestSnapshotRetentionProtects_PinExpiredNeverSetOnASurvivingCopy(t *testing.T) {
	floor := newRetentionFloor(2)
	completeKept := false

	// Kept by newest-N even though the pin window has passed.
	if got := snapshotRetentionProtects(floor, &completeKept, true, true, false); got.keep != retentionKeepNewest || got.pinExpired {
		t.Fatalf("keep=%v pinExpired=%v, want kept by newest-N with no expiry warning", got.keep, got.pinExpired)
	}
	// Kept by the pin itself.
	floorFull := newRetentionFloor(0)
	completeKept2 := false
	if got := snapshotRetentionProtects(floorFull, &completeKept2, true, true, true); got.keep != retentionKeepNewestComplete || got.pinExpired {
		t.Fatalf("keep=%v pinExpired=%v, want kept by the pin with no expiry warning", got.keep, got.pinExpired)
	}
	// A degraded copy is not the complete archive, so its removal is ordinary.
	if got := snapshotRetentionProtects(floorFull, &completeKept2, true, false, false); got.keep != retentionKeepNone || got.pinExpired {
		t.Fatalf("keep=%v pinExpired=%v, want an unremarkable delete", got.keep, got.pinExpired)
	}
}

// The grace is ADDED to the tier's own window, not multiplied by it, so a long
// offsite policy cannot silently pin a second one.
func TestCompletePinCutoff_AddsGraceToTheRetentionWindow(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		cleanupDays int
		wantDays    int
	}{
		{cleanupDays: 2, wantDays: 2 + completePinGraceDays},
		{cleanupDays: 30, wantDays: 30 + completePinGraceDays},
		{cleanupDays: 365, wantDays: 365 + completePinGraceDays},
	} {
		got := completePinCutoff(now, tc.cleanupDays)
		want := now.AddDate(0, 0, -tc.wantDays)
		if !got.Equal(want) {
			t.Errorf("cleanupDays=%d: cutoff %s, want %s", tc.cleanupDays, got, want)
		}
		// The pin must always outlast the retention window it supplements,
		// otherwise it could never keep anything age-based retention deletes.
		if !got.Before(now.AddDate(0, 0, -tc.cleanupDays)) {
			t.Errorf("cleanupDays=%d: pin cutoff %s is not older than the retention cutoff", tc.cleanupDays, got)
		}
	}
}

func TestSnapshotIsComplete_UnknownIsComplete(t *testing.T) {
	if !snapshotIsComplete(&snapshotstore.Record{}) {
		t.Fatal("a pre-019 row (Instances nil) must count as complete so retention stays conservative")
	}
	if snapshotIsComplete(&snapshotstore.Record{Instances: []snapshotstore.RecordInstance{
		{Name: "a", HasData: true},
		{Name: "b", HasData: false},
	}}) {
		t.Fatal("an archive with a config-only instance is not complete")
	}
	if !snapshotIsComplete(&snapshotstore.Record{Instances: []snapshotstore.RecordInstance{
		{Name: "a", HasData: true},
	}}) {
		t.Fatal("every instance having data is complete")
	}
}

func TestRetentionFloor_ProtectsExactlyTheNewestN(t *testing.T) {
	floor := newRetentionFloor(2)

	for i, want := range []bool{true, true, false, false} {
		if got := floor.protects(true); got != want {
			t.Errorf("record %d: protects = %v, want %v", i, got, want)
		}
	}
}

// Once the quota is met the answer cannot change, so offsite retention skips the
// HeadObject entirely — that is what keeps this to a couple of requests per run.
func TestRetentionFloor_FullShortCircuitsExistenceChecks(t *testing.T) {
	floor := newRetentionFloor(1)
	if floor.full() {
		t.Fatal("an empty floor reports itself full")
	}
	floor.protects(true)
	if !floor.full() {
		t.Error("floor with its quota met does not report full")
	}
}

func TestLocalArchivePresent(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "snapshot.tar.zst")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		dataDir  string
		location string
		want     bool
	}{
		{"absolute path that exists", "", real, true},
		{"absolute path that does not", "", filepath.Join(dir, "gone.tar.zst"), false},
		{"relative path resolved against the data dir", dir, "snapshot.tar.zst", true},
		{"relative path that does not exist", dir, "gone.tar.zst", false},
		{"no location at all", "", "", false},
		// FAILS SAFE: a stat that fails for any reason other than "not there"
		// must report present. ENOTDIR (a path component that is a file) is the
		// deterministic stand-in for the real cases — EACCES, EIO, a sick mount.
		// Retention must never become more willing to delete because the
		// filesystem is broken.
		{"stat fails for a reason other than absence", "", filepath.Join(real, "nested"), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := localArchivePresent(tc.dataDir, tc.location); got != tc.want {
				t.Errorf("localArchivePresent(%q, %q) = %v, want %v", tc.dataDir, tc.location, got, tc.want)
			}
		})
	}
}

// Both of these return before the S3 client is touched (hence the nil), and both
// must report present: neither is a record whose object we know to be gone, and
// unprotecting the floor on a location we cannot even evaluate would be the
// aggressive direction.
func TestRemoteArchivePresent_UnevaluableLocationsCountAsPresent(t *testing.T) {
	ctx := context.Background()

	if !remoteArchivePresent(ctx, nil, "bucket", "/var/lib/oddk/backups/snap.tar.zst") {
		t.Error("an unparseable remote location was treated as absent")
	}
	if !remoteArchivePresent(ctx, nil, "bucket", "s3://someone-elses-bucket/key.tar.zst") {
		t.Error("an archive in another bucket was treated as absent; it is not ours to check or delete")
	}
}
