package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// testSnapshotMigrateFromBackups covers `oddk snapshot migrate-from-backups`
// against the real daemon. The command is client-side, so unit tests already
// cover its logic against a fake daemon; what only an e2e run can prove is that
// the four endpoints it composes actually behave the way it assumes — in
// particular that DELETE /api/cron/backup/{n} really removes the plan and that
// POST /api/cron/snapshot stores what was derived.
//
// Two instances with DIFFERENT hours and DIFFERENT retention windows, so the
// derivation is actually exercised rather than trivially satisfied.
func testSnapshotMigrateFromBackups(h *TestHarness) error {
	log.Println("=== Testing Snapshot Migrate From Backups ===")

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	const (
		portA = 15495
		portB = 15496
	)
	stamp := time.Now().Unix()
	nameA := fmt.Sprintf("oddk-danger-funct-mig-a-%d", stamp)
	nameB := fmt.Sprintf("oddk-danger-funct-mig-b-%d", stamp)

	log.Println("Step 1: Two instances, each with its own backup schedule")
	for _, inst := range []struct {
		name string
		port int
	}{{nameA, portA}, {nameB, portB}} {
		output, err := h.runCLI("create",
			"--name", inst.name, "--version", "17",
			"--port", strconv.Itoa(inst.port), "--cpu", "1", "--ram", "512M")
		if err != nil {
			return fmt.Errorf("create %s: %w (output: %s)", inst.name, err, output)
		}
		defer func(n string) { _, _ = h.runCLI("instance", "destroy", n, "--force") }(inst.name)
		if err := h.waitForPostgreSQL(inst.port); err != nil {
			return fmt.Errorf("%s not ready: %w", inst.name, err)
		}
	}

	// Hours disagree (2 vs 5) and retention differs (7/14 vs 21/40).
	output, err := h.runCLI("backup", "setup-cron", "--instance", nameA,
		"--utc-hour", "2", "--cleanup-local-days", "7", "--cleanup-remote-days", "14")
	if err != nil {
		return fmt.Errorf("setup backup cron for %s: %w (output: %s)", nameA, err, output)
	}
	output, err = h.runCLI("backup", "setup-cron", "--instance", nameB,
		"--utc-hour", "5", "--cleanup-local-days", "21", "--cleanup-remote-days", "40")
	if err != nil {
		return fmt.Errorf("setup backup cron for %s: %w (output: %s)", nameB, err, output)
	}

	log.Println("Step 2: Dry run previews the derivation and changes nothing")
	output, err = h.runCLI("snapshot", "migrate-from-backups", "--dry-run")
	if err != nil {
		return fmt.Errorf("dry run: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Dry run: nothing was changed.") {
		return fmt.Errorf("dry run did not say so; got: %s", output)
	}
	// Retention must be the LONGEST window any plan used — silently shortening
	// someone's retention is the one outcome of this migration that cannot be
	// undone.
	for _, want := range []string{"Keep local:  21 days", "Keep offsite: 40 days", "did not agree on an hour"} {
		if !strings.Contains(output, want) {
			return fmt.Errorf("dry run output missing %q; got: %s", want, output)
		}
	}
	// Nothing may have been written.
	output, err = h.runCLI("snapshot", "list-cron")
	if err != nil {
		return fmt.Errorf("list-cron after dry run: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "No scheduled snapshots configured") {
		return fmt.Errorf("dry run created a snapshot schedule; got: %s", output)
	}
	output, err = h.runCLI("backup", "list-cron")
	if err != nil {
		return fmt.Errorf("backup list-cron after dry run: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, nameA) || !strings.Contains(output, nameB) {
		return fmt.Errorf("dry run removed a backup schedule; got: %s", output)
	}

	log.Println("Step 3: Migrating for real")
	output, err = h.runCLI("snapshot", "migrate-from-backups", "--yes")
	if err != nil {
		return fmt.Errorf("migrate: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Removed 2 backup schedule(s)") {
		return fmt.Errorf("migrate did not report removing both schedules; got: %s", output)
	}

	// The daemon must actually hold the derived plan.
	output, err = h.runCLI("snapshot", "list-cron", "--json")
	if err != nil {
		return fmt.Errorf("list-cron --json: %w (output: %s)", err, output)
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
		return fmt.Errorf("parse list-cron --json (%w): %s", err, output)
	}
	if wrapper.Plan == nil {
		return fmt.Errorf("no snapshot schedule was stored; got: %s", output)
	}
	if wrapper.Plan.UTCHour != 2 {
		return fmt.Errorf("expected the earliest of two tied-frequency hours (02), got %02d", wrapper.Plan.UTCHour)
	}
	if wrapper.Plan.IntervalHours != 24 {
		return fmt.Errorf("expected daily (24) to match backup schedules, got %d", wrapper.Plan.IntervalHours)
	}
	if wrapper.Plan.CleanupLocalDays != 21 || wrapper.Plan.CleanupRemoteDays != 40 {
		return fmt.Errorf("retention must be the longest window any plan used (21/40), got %d/%d",
			wrapper.Plan.CleanupLocalDays, wrapper.Plan.CleanupRemoteDays)
	}

	// And the backup schedules must really be gone from the daemon's store.
	output, err = h.runCLI("backup", "list-cron")
	if err != nil {
		return fmt.Errorf("backup list-cron after migrate: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "No scheduled backups configured") {
		return fmt.Errorf("backup schedules survived the migration; got: %s", output)
	}

	log.Println("Step 4: The checklist stops flagging instances that snapshots now cover")
	output, err = h.runCLI("checklist")
	if err != nil {
		return fmt.Errorf("checklist: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "covered by scheduled snapshots") {
		return fmt.Errorf("checklist should report instances as covered by snapshots rather than "+
			"as an outstanding task, otherwise every migrated host reads as broken; got: %s", output)
	}
	if strings.Contains(output, "daily backup     not scheduled") {
		return fmt.Errorf("checklist still reports 'not scheduled' after migration; got: %s", output)
	}

	log.Println("Step 5: Re-running on a migrated host is a quiet success")
	output, err = h.runCLI("snapshot", "migrate-from-backups", "--yes")
	if err != nil {
		return fmt.Errorf("re-running on a migrated host must not error (a fleet rollout re-runs): "+
			"%w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Already migrated") {
		return fmt.Errorf("expected an already-migrated notice; got: %s", output)
	}
	// The second run must not have disturbed the schedule it found.
	output, err = h.runCLI("snapshot", "list-cron", "--json")
	if err != nil {
		return fmt.Errorf("list-cron after re-run: %w (output: %s)", err, output)
	}
	wrapper.Plan = nil
	if err := json.Unmarshal([]byte(output), &wrapper); err != nil {
		return fmt.Errorf("parse list-cron --json (%w): %s", err, output)
	}
	if wrapper.Plan == nil || wrapper.Plan.CleanupLocalDays != 21 {
		return fmt.Errorf("re-running changed the stored schedule; got: %s", output)
	}

	log.Println("Step 6: Cleaning up the schedule")
	if output, err := h.runCLI("snapshot", "setup-cron", "--remove"); err != nil {
		return fmt.Errorf("remove snapshot schedule: %w (output: %s)", err, output)
	}

	log.Println("=== Snapshot Migrate From Backups test passed ===")
	return nil
}
