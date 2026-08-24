package operations

import (
	"testing"
)

// The hazard: the restored oddk.db carries the SOURCE host's schedules and its
// offsite credentials together, so an unpaused restore uploads into — and runs
// retention against — a bucket the source may still own, deleting the SOURCE's
// archives.
func TestPauseRestoredSchedules_PausesEverythingByDefault(t *testing.T) {
	st, _ := newTestStore(t)

	if err := st.Snapshot.SetPlan(3, 24, 7, 14, "physical"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if err := st.Cron.CreatePlan(name, 3, 7, 14); err != nil {
			t.Fatal(err)
		}
	}

	snapshotPaused, backupsPaused, err := pauseRestoredSchedules(st, false)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshotPaused {
		t.Error("the snapshot schedule must be paused")
	}
	if backupsPaused != 2 {
		t.Errorf("paused %d backup schedules, want 2", backupsPaused)
	}

	snapPlan, err := st.Snapshot.GetPlan()
	if err != nil {
		t.Fatal(err)
	}
	if !snapPlan.IsPaused() {
		t.Error("snapshot plan is not paused in the store")
	}
	if snapPlan.PausedReason == "" {
		t.Error("a pause with no reason tells the operator nothing about why it happened")
	}
	// Pausing must not disturb the schedule — that is why it beats deleting.
	if snapPlan.UTCHour != 3 || snapPlan.CleanupLocalDays != 7 || snapPlan.Format != "physical" {
		t.Errorf("pause altered the restored schedule: %+v", snapPlan)
	}

	// Legacy per-instance backup crons upload to and prune the SAME bucket, so
	// pausing only snapshots would leave the collision open on exactly the
	// deployments that have not run 'snapshot migrate-from-backups'.
	backupPlans, err := st.Cron.ListPausedPlans()
	if err != nil {
		t.Fatal(err)
	}
	if len(backupPlans) != 2 {
		t.Errorf("%d backup plans paused in the store, want 2", len(backupPlans))
	}
}

// --no-pause-schedules is the operator asserting the source is gone. It must
// leave the plans exactly as the archive carried them.
func TestPauseRestoredSchedules_NoPauseLeavesPlansAlone(t *testing.T) {
	st, _ := newTestStore(t)

	if err := st.Snapshot.SetPlan(3, 24, 7, 14, "physical"); err != nil {
		t.Fatal(err)
	}
	if err := st.Cron.CreatePlan("alpha", 3, 7, 14); err != nil {
		t.Fatal(err)
	}

	snapshotPaused, backupsPaused, err := pauseRestoredSchedules(st, true)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotPaused || backupsPaused != 0 {
		t.Fatalf("reported snapshotPaused=%v backupsPaused=%d, want nothing paused", snapshotPaused, backupsPaused)
	}

	snapPlan, err := st.Snapshot.GetPlan()
	if err != nil {
		t.Fatal(err)
	}
	if snapPlan.IsPaused() {
		t.Error("--no-pause-schedules must not pause the snapshot schedule")
	}
	paused, err := st.Cron.ListPausedPlans()
	if err != nil {
		t.Fatal(err)
	}
	if len(paused) != 0 {
		t.Errorf("%d backup plans paused, want 0", len(paused))
	}
}

// A deployment that never scheduled a snapshot is the ordinary case, not an
// error: ErrNoSnapshotPlan must not abort an apply that is otherwise complete.
func TestPauseRestoredSchedules_NoScheduleIsNotAnError(t *testing.T) {
	st, _ := newTestStore(t)

	snapshotPaused, backupsPaused, err := pauseRestoredSchedules(st, false)
	if err != nil {
		t.Fatalf("apply must not fail when the archive carried no schedules: %v", err)
	}
	if snapshotPaused || backupsPaused != 0 {
		t.Errorf("reported snapshotPaused=%v backupsPaused=%d with nothing to pause", snapshotPaused, backupsPaused)
	}
}

// The --no-pause-schedules warning must describe something real. Keyed off the
// flag alone it would fire on a deployment whose archive carried no schedule at
// all — and a warning that cries wolf on the harmless case gets skimmed past on
// the dangerous one.
func TestCountActiveRestoredSchedules_OnlyCountsWhatWillActuallyRun(t *testing.T) {
	st, _ := newTestStore(t)

	snapshotActive, backupsActive, err := countActiveRestoredSchedules(st)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotActive || backupsActive != 0 {
		t.Fatalf("empty deployment reported snapshot=%v backups=%d, want nothing active", snapshotActive, backupsActive)
	}

	if err := st.Snapshot.SetPlan(3, 24, 7, 14, "physical"); err != nil {
		t.Fatal(err)
	}
	if err := st.Cron.CreatePlan("alpha", 3, 7, 14); err != nil {
		t.Fatal(err)
	}
	if err := st.Cron.CreatePlan("beta", 4, 7, 14); err != nil {
		t.Fatal(err)
	}
	snapshotActive, backupsActive, err = countActiveRestoredSchedules(st)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshotActive || backupsActive != 2 {
		t.Fatalf("snapshot=%v backups=%d, want the snapshot plan plus 2 backup plans", snapshotActive, backupsActive)
	}

	// A plan the SOURCE had already paused is not "left running":
	// --no-pause-schedules skips the pause step, it does not resume anything.
	if err := st.Snapshot.PausePlan("paused on the source host"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Cron.PausePlan("alpha", "paused on the source host"); err != nil {
		t.Fatal(err)
	}
	snapshotActive, backupsActive, err = countActiveRestoredSchedules(st)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotActive {
		t.Error("a snapshot plan the source had paused must not count as left running")
	}
	if backupsActive != 1 {
		t.Errorf("backups active = %d, want only the one that was not already paused", backupsActive)
	}
}
