//go:build oddk_debug

package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// testBackupScheduleDeprecation covers the one behaviour the backup
// deprecation changes: the daemon refuses to CREATE a per-instance backup
// schedule, while every operation on an EXISTING one keeps working — a
// deployment that has not migrated yet must not be stranded with a schedule it
// cannot change.
//
// Runs with the harness's opt-in switch OFF (the production behaviour), and
// flips it on only to create the pre-existing schedule.
func testBackupScheduleDeprecation(h *TestHarness) error {
	log.Println("=== Testing Backup Schedule Deprecation ===")

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}
	const port = 15516
	instanceName := fmt.Sprintf("oddk-danger-funct-bdep-%d", time.Now().Unix())
	output, err := h.runCLI("create", "--name", instanceName, "--version", "17",
		"--port", strconv.Itoa(port), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance: %w (output: %s)", err, output)
	}
	defer func() { _, _ = h.runCLI("instance", "destroy", instanceName, "--force") }()

	log.Println("Step 1: A new backup schedule is refused, pointing at snapshots")
	output, err = h.runCLI("backup", "setup-cron", "--instance", instanceName, "--utc-hour", "3")
	if err == nil {
		return fmt.Errorf("a new backup schedule was accepted; output: %s", output)
	}
	for _, want := range []string{"no longer accepted", "2026-12-31", "oddk snapshot setup-cron", "migrate-from-backups"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
	if output, err = h.runCLI("backup", "list-cron"); err != nil || strings.Contains(output, instanceName) {
		return fmt.Errorf("a refused schedule was stored anyway (%v): %s", err, output)
	}

	log.Println("Step 2: An EXISTING schedule can still be changed, paused, resumed and removed")
	if err := h.server.DebugSetRawKV(debugAllowNewBackupPlansKey, "1"); err != nil {
		return err
	}
	if output, err = h.runCLI("backup", "setup-cron", "--instance", instanceName, "--utc-hour", "3"); err != nil {
		return fmt.Errorf("create the pre-existing schedule: %w (output: %s)", err, output)
	}
	if err := h.server.DebugSetRawKV(debugAllowNewBackupPlansKey, "0"); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"--utc-hour", "5"},
		{"--cleanup-local-days", "10"},
		{"--pause"},
		{"--resume"},
		{"--remove"},
	} {
		cmd := append([]string{"backup", "setup-cron", "--instance", instanceName}, args...)
		if output, err = h.runCLI(cmd...); err != nil {
			return fmt.Errorf("backup setup-cron %v on an existing schedule was refused: %w (output: %s)", args, err, output)
		}
	}

	log.Println("Step 3: Once removed, it cannot be re-created")
	if _, err = h.runCLI("backup", "setup-cron", "--instance", instanceName, "--utc-hour", "3"); err == nil {
		return fmt.Errorf("a removed backup schedule could be re-created")
	}

	log.Println("=== Backup Schedule Deprecation Test PASSED ===")
	return nil
}
