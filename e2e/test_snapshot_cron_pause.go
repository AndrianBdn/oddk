//go:build oddk_debug

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"
)

// testSnapshotCronPause covers suspending a schedule without deleting it.
//
// The guarantee that matters is the negative one: a PAUSED plan must not fire
// even under cron.debug_force_run, which otherwise runs every plan with 100%
// probability every tick. That flag exists to make tests fire a schedule
// immediately; it must not defeat the safeguard that keeps a DR-rehearsal host
// out of a live source deployment's offsite bucket.
//
// Runs with no instances, like testSnapshotCron: everything under test here is
// scheduling, not capture.
func testSnapshotCronPause(h *TestHarness) error {
	log.Println("=== Testing Snapshot Schedule Pause/Resume ===")

	backupDir := filepath.Join(h.dataDir, "backups")

	// The scheduler dedups on the START OF THE SLOT (the top of the current
	// hour), so once a snapshot has run this hour no other can, paused or not.
	// A test that let one run first and then asserted "nothing more happened"
	// would pass even if pausing did nothing. So: no run happens at all until
	// the resume, and force-run is held OFF while the plan is being created and
	// paused, which removes the race between setup-cron and --pause.
	log.Println("Step 1: Hold the scheduler back, then create a schedule and pause it")
	if output, err := h.runCLI("customkv", "set", "cron.debug_force_run.int", "--value", "0"); err != nil {
		return fmt.Errorf("disable force-run: %w (output: %s)", err, output)
	}
	// Anchor far from the current hour so the plan cannot fire on its own merits
	// while force-run is off.
	anchor := (time.Now().UTC().Hour() + 12) % 24
	if output, err := h.runCLI("snapshot", "setup-cron", "--utc-hour", fmt.Sprintf("%d", anchor)); err != nil {
		return fmt.Errorf("setup-cron: %w (output: %s)", err, output)
	}
	output, err := h.runCLI("snapshot", "setup-cron", "--pause")
	if err != nil {
		return fmt.Errorf("setup-cron --pause: %w (output: %s)", err, output)
	}
	if !strings.Contains(strings.ToLower(output), "paused") {
		return fmt.Errorf("pause output should say so, got: %s", output)
	}

	log.Println("Step 2: Pausing keeps the schedule, it does not delete it")
	output, err = h.runCLI("snapshot", "list-cron", "--json")
	if err != nil {
		return fmt.Errorf("list-cron --json: %w", err)
	}
	var wrapper struct {
		Plan *struct {
			UTCHour       int    `json:"utcHour"`
			IntervalHours int    `json:"intervalHours"`
			PausedAt      string `json:"pausedAt"`
			PausedReason  string `json:"pausedReason"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(output), &wrapper); err != nil {
		return fmt.Errorf("parse list-cron json %q: %w", output, err)
	}
	if wrapper.Plan == nil {
		return fmt.Errorf("pausing must not delete the schedule")
	}
	if wrapper.Plan.PausedAt == "" {
		return fmt.Errorf("plan should report pausedAt, got %+v", wrapper.Plan)
	}
	if wrapper.Plan.UTCHour != anchor {
		return fmt.Errorf("pause altered the schedule: %+v (anchor should still be %d)", wrapper.Plan, anchor)
	}

	log.Println("Step 3: The checklist reports it as a PROBLEM, not as protection")
	output, err = h.runCLI("checklist")
	if err != nil {
		return fmt.Errorf("checklist: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "PAUSED") {
		return fmt.Errorf("checklist must surface a paused schedule; got: %s", output)
	}
	if !strings.Contains(output, "--resume") {
		return fmt.Errorf("checklist should tell the operator how to resume; got: %s", output)
	}

	log.Println("Step 4: A paused plan does NOT fire, even under force-run")
	if output, err := h.runCLI("customkv", "set", "cron.debug_force_run.int", "--value", "1"); err != nil {
		return fmt.Errorf("re-enable force-run: %w (output: %s)", err, output)
	}
	// force-run + the 1s debug ticker fires an eligible plan within seconds, and
	// nothing has run in this slot, so the pause is the ONLY thing that can be
	// holding it back.
	time.Sleep(20 * time.Second)
	archives, _ := filepath.Glob(filepath.Join(backupDir, "snapshot-*.tar.zst"))
	if len(archives) != 0 {
		return fmt.Errorf("a PAUSED schedule fired anyway (%d archive(s)) — force-run must not defeat the pause", len(archives))
	}

	log.Println("Step 5: Resume, and the same schedule fires")
	output, err = h.runCLI("snapshot", "setup-cron", "--resume")
	if err != nil {
		return fmt.Errorf("setup-cron --resume: %w (output: %s)", err, output)
	}
	if err := waitForSnapshotArchives(backupDir, 1, 90*time.Second); err != nil {
		return fmt.Errorf("after resume: %w", err)
	}

	output, err = h.runCLI("checklist")
	if err != nil {
		return fmt.Errorf("checklist after resume: %w", err)
	}
	if strings.Contains(output, "PAUSED") {
		return fmt.Errorf("checklist still reports PAUSED after resume; got: %s", output)
	}

	log.Println("✅ Snapshot schedule pause/resume works")
	return nil
}

func waitForSnapshotArchives(backupDir string, want int, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		archives, _ := filepath.Glob(filepath.Join(backupDir, "snapshot-*.tar.zst"))
		if len(archives) >= want {
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	archives, _ := filepath.Glob(filepath.Join(backupDir, "snapshot-*.tar.zst"))
	return fmt.Errorf("expected at least %d snapshot archive(s) within %s, have %d", want, within, len(archives))
}
