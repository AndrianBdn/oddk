package main

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"
)

// testSnapshotCron covers the deployment-wide snapshot schedule: configuring it,
// the scheduler actually firing it, the catalogue record it produces, and the
// audit row in cron_logs.
//
// Deliberately runs with NO instances. A snapshot of an empty deployment still
// exercises everything this feature adds — plan storage, the singleton, the
// interval schedule, the scheduler dispatch under the sentinel name, the
// catalogue record written after the archive exists — while keeping the test
// fast and independent of PostgreSQL container startup.
func testSnapshotCron(h *TestHarness) error {
	log.Println("=== Testing Scheduled Snapshots ===")

	log.Println("Step 1: No schedule configured initially")
	output, err := h.runCLI("snapshot", "list-cron")
	if err != nil {
		return fmt.Errorf("list-cron on a fresh deployment: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "No scheduled snapshots configured") {
		return fmt.Errorf("expected 'not configured', got: %s", output)
	}

	log.Println("Step 2: Rejecting an interval that does not divide 24")
	output, err = h.runCLI("snapshot", "setup-cron", "--utc-hour", "3", "--interval-hours", "5")
	if err == nil {
		return fmt.Errorf("interval 5 should be rejected (it drifts across midnight); output: %s", output)
	}
	if !strings.Contains(err.Error(), "divide 24") {
		return fmt.Errorf("expected an explanation about dividing 24, got: %v (output: %s)", err, output)
	}

	log.Println("Step 3: Configuring the schedule")
	output, err = h.runCLI("snapshot", "setup-cron",
		"--utc-hour", "3", "--interval-hours", "6",
		"--cleanup-local-days", "2", "--cleanup-remote-days", "5")
	if err != nil {
		return fmt.Errorf("setup-cron: %w (output: %s)", err, output)
	}
	// The anchored hours must be spelled out - "every 6 hours" alone does not
	// say which hours, and getting that wrong is silent.
	for _, want := range []string{"03:00", "09:00", "15:00", "21:00"} {
		if !strings.Contains(output, want) {
			return fmt.Errorf("setup-cron output does not list run hour %s; got: %s", want, output)
		}
	}

	// Re-configuring must UPDATE the single plan, never add a second one — and
	// must preserve fields the operator did not mention. Moving the anchor is
	// not a request to change the frequency, and silently reverting a 6-hour
	// interval to daily would be a schedule nobody asked for.
	if output, err = h.runCLI("snapshot", "setup-cron", "--utc-hour", "4"); err != nil {
		return fmt.Errorf("re-running setup-cron: %w (output: %s)", err, output)
	}
	output, err = h.runCLI("snapshot", "list-cron", "--json")
	if err != nil {
		return fmt.Errorf("list-cron --json: %w", err)
	}
	var wrapper struct {
		Plan *struct {
			UTCHour           int `json:"utcHour"`
			IntervalHours     int `json:"intervalHours"`
			CleanupLocalDays  int `json:"cleanupLocalDays"`
			CleanupRemoteDays int `json:"cleanupRemoteDays"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(output), &wrapper); err != nil {
		return fmt.Errorf("parse list-cron json %q: %w", output, err)
	}
	if wrapper.Plan == nil {
		return fmt.Errorf("no plan after setup-cron")
	}
	if wrapper.Plan.UTCHour != 4 {
		return fmt.Errorf("plan = %+v, want the new anchor hour 4", wrapper.Plan)
	}
	if wrapper.Plan.IntervalHours != 6 {
		return fmt.Errorf("plan = %+v: the 6-hour interval was reset by a --utc-hour-only update; unspecified fields must be preserved", wrapper.Plan)
	}
	if wrapper.Plan.CleanupLocalDays != 2 || wrapper.Plan.CleanupRemoteDays != 5 {
		return fmt.Errorf("plan = %+v: retention was reset by a --utc-hour-only update", wrapper.Plan)
	}

	log.Println("Step 4: Waiting for the scheduler to fire it (force-run mode)")
	backupDir := filepath.Join(h.dataDir, "backups")
	deadline := time.Now().Add(60 * time.Second)
	var archives []string
	for time.Now().Before(deadline) {
		archives, _ = filepath.Glob(filepath.Join(backupDir, "snapshot-*.tar.zst"))
		if len(archives) > 0 {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if len(archives) == 0 {
		return fmt.Errorf("scheduler did not produce a snapshot archive within 60s")
	}

	log.Println("Step 5: The snapshot is in the catalogue")
	// Give the catalogue write a moment after the archive appears.
	var records []struct {
		ID            int    `json:"id"`
		Filename      string `json:"filename"`
		Status        string `json:"status"`
		Comment       string `json:"comment"`
		LocalLocation string `json:"localLocation"`
	}
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		output, err = h.runCLI("snapshot", "list", "--json")
		if err != nil {
			return fmt.Errorf("snapshot list: %w (output: %s)", err, output)
		}
		if err := json.Unmarshal([]byte(output), &records); err != nil {
			return fmt.Errorf("parse snapshot list %q: %w", output, err)
		}
		if len(records) > 0 {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if len(records) == 0 {
		return fmt.Errorf("snapshot archive exists but nothing was recorded in the catalogue")
	}
	rec := records[0]
	if rec.Status != "completed" {
		return fmt.Errorf("catalogue status = %q, want completed", rec.Status)
	}
	if rec.Comment != "scheduled" {
		return fmt.Errorf("catalogue comment = %q, want 'scheduled' so a scheduled run is distinguishable", rec.Comment)
	}
	if rec.LocalLocation == "" {
		return fmt.Errorf("catalogue record has no local location")
	}

	log.Println("Step 6: The sentinel must not leak into the per-instance cron listing")
	// A snapshot rides the shared task queue under a sentinel name. That name is
	// deliberately unrepresentable as an instance, so it must never surface where
	// per-instance backup plans are listed.
	logsOut, err := h.runCLI("backup", "list-cron")
	if err != nil {
		return fmt.Errorf("backup list-cron: %w (output: %s)", err, logsOut)
	}
	if strings.Contains(logsOut, "*snapshot*") {
		return fmt.Errorf("the snapshot sentinel leaked into 'backup list-cron', which lists per-instance plans: %s", logsOut)
	}

	log.Println("Step 6b: The checklist reports the snapshot state")
	// The checklist is the audit view. Reporting healthy per-instance backups
	// while saying nothing about snapshots would describe a deployment as fine
	// when it may have nothing to rebuild a host from.
	checklistOut, err := h.runCLI("checklist")
	if err != nil {
		return fmt.Errorf("checklist: %w (output: %s)", err, checklistOut)
	}
	if !strings.Contains(checklistOut, "Snapshots (whole deployment)") {
		return fmt.Errorf("checklist has no snapshot section: %s", checklistOut)
	}
	if !strings.Contains(checklistOut, "scheduled:") {
		return fmt.Errorf("checklist does not report the snapshot schedule: %s", checklistOut)
	}
	if !strings.Contains(checklistOut, "last snapshot:") {
		return fmt.Errorf("checklist does not report the last snapshot: %s", checklistOut)
	}

	log.Println("Step 7: Removing the schedule")
	if output, err = h.runCLI("snapshot", "setup-cron", "--remove"); err != nil {
		return fmt.Errorf("setup-cron --remove: %w (output: %s)", err, output)
	}
	output, err = h.runCLI("snapshot", "list-cron")
	if err != nil {
		return fmt.Errorf("list-cron after removal: %w", err)
	}
	if !strings.Contains(output, "No scheduled snapshots configured") {
		return fmt.Errorf("schedule survived removal: %s", output)
	}

	log.Println("=== Scheduled Snapshots Test PASSED ===")
	return nil
}
