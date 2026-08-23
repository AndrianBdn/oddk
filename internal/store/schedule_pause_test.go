package store_test

import (
	"errors"
	"testing"

	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
)

// Pausing exists because `oddk snapshot apply` restores the SOURCE host's
// oddk.db — schedule and offsite settings together — so an unpaused restore
// uploads into, and runs retention against, a bucket the source may still own.
// Deleting the schedule (the previous workaround) trades that for a worse
// failure: a real DR host nobody re-schedules is permanently unprotected.

func TestSnapshotPlanPauseResumeRoundTrip(t *testing.T) {
	st := newTestStore(t)

	if err := st.Snapshot.SetPlan(3, 6, 7, 14, "physical"); err != nil {
		t.Fatal(err)
	}
	plan, err := st.Snapshot.GetPlan()
	if err != nil {
		t.Fatal(err)
	}
	if plan.IsPaused() {
		t.Fatal("a freshly created plan must not be paused")
	}

	if err := st.Snapshot.PausePlan("suspended by 'snapshot apply'"); err != nil {
		t.Fatal(err)
	}
	plan, err = st.Snapshot.GetPlan()
	if err != nil {
		t.Fatal(err)
	}
	if !plan.IsPaused() {
		t.Fatal("plan should be paused")
	}
	if plan.PausedReason != "suspended by 'snapshot apply'" {
		t.Fatalf("reason = %q", plan.PausedReason)
	}
	// Pausing must not disturb the schedule itself — that is the whole point of
	// pausing rather than deleting.
	if plan.UTCHour != 3 || plan.IntervalHours != 6 || plan.CleanupLocalDays != 7 || plan.Format != "physical" {
		t.Fatalf("pause altered the schedule: %+v", plan)
	}

	if err := st.Snapshot.ResumePlan(); err != nil {
		t.Fatal(err)
	}
	plan, err = st.Snapshot.GetPlan()
	if err != nil {
		t.Fatal(err)
	}
	if plan.IsPaused() || plan.PausedReason != "" {
		t.Fatalf("resume must clear both fields, got paused=%v reason=%q", plan.IsPaused(), plan.PausedReason)
	}
}

// SetPlan is an upsert. Adjusting the hour of a paused schedule must NOT resume
// it: on a rehearsal host that would silently rejoin the live source's bucket as
// a side effect of an unrelated edit.
func TestSnapshotSetPlanDoesNotResume(t *testing.T) {
	st := newTestStore(t)

	if err := st.Snapshot.SetPlan(3, 24, 7, 14, "physical"); err != nil {
		t.Fatal(err)
	}
	if err := st.Snapshot.PausePlan("dr rehearsal"); err != nil {
		t.Fatal(err)
	}
	if err := st.Snapshot.SetPlan(5, 24, 7, 14, "physical"); err != nil {
		t.Fatal(err)
	}

	plan, err := st.Snapshot.GetPlan()
	if err != nil {
		t.Fatal(err)
	}
	if plan.UTCHour != 5 {
		t.Fatalf("the hour should have been updated, got %d", plan.UTCHour)
	}
	if !plan.IsPaused() {
		t.Fatal("editing a field must NOT resume a paused schedule; resuming has to be deliberate")
	}
}

func TestSnapshotPauseWithoutAPlanIsReported(t *testing.T) {
	st := newTestStore(t)

	if err := st.Snapshot.PausePlan("x"); !errors.Is(err, snapshotstore.ErrNoSnapshotPlan) {
		t.Fatalf("pausing with no schedule should report it, got %v", err)
	}
	if err := st.Snapshot.ResumePlan(); !errors.Is(err, snapshotstore.ErrNoSnapshotPlan) {
		t.Fatalf("resuming with no schedule should report it, got %v", err)
	}
}

func TestBackupPlanPauseResume(t *testing.T) {
	st := newTestStore(t)

	if err := st.Cron.CreatePlan("app", 3, 7, 14); err != nil {
		t.Fatal(err)
	}
	if err := st.Cron.CreatePlan("other", 3, 7, 14); err != nil {
		t.Fatal(err)
	}

	n, err := st.Cron.PauseAllPlans("suspended by 'snapshot apply'")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("paused %d plans, want 2", n)
	}

	// The scheduler's own lookup must still return a paused plan, carrying its
	// state: the SCHEDULER decides to skip it. Filtering in SQL would also hide
	// it from list-cron and the checklist, which are how an operator finds out
	// it needs resuming.
	forHour, err := st.Cron.GetPlansForHour(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(forHour) != 2 {
		t.Fatalf("hour lookup returned %d plans, want 2", len(forHour))
	}
	for _, p := range forHour {
		if !p.IsPaused() {
			t.Fatalf("plan %s should carry its paused state to the scheduler", p.InstanceName)
		}
	}

	paused, err := st.Cron.ListPausedPlans()
	if err != nil {
		t.Fatal(err)
	}
	if len(paused) != 2 {
		t.Fatalf("ListPausedPlans returned %d", len(paused))
	}

	if _, err := st.Cron.ResumePlan("app"); err != nil {
		t.Fatal(err)
	}
	paused, err = st.Cron.ListPausedPlans()
	if err != nil {
		t.Fatal(err)
	}
	if len(paused) != 1 || paused[0].InstanceName != "other" {
		t.Fatalf("resume should affect exactly one plan, got %+v", paused)
	}
}

// CreatePlan is an upsert too, and carries the same rule as SetPlan.
func TestBackupCreatePlanDoesNotResume(t *testing.T) {
	st := newTestStore(t)

	if err := st.Cron.CreatePlan("app", 3, 7, 14); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Cron.PauseAllPlans("dr rehearsal"); err != nil {
		t.Fatal(err)
	}
	if err := st.Cron.CreatePlan("app", 5, 7, 14); err != nil {
		t.Fatal(err)
	}

	plan, err := st.Cron.GetPlan("app")
	if err != nil {
		t.Fatal(err)
	}
	if plan.UTCHour != 5 {
		t.Fatalf("hour = %d, want 5", plan.UTCHour)
	}
	if !plan.IsPaused() {
		t.Fatal("editing a field must NOT resume a paused schedule")
	}
}
