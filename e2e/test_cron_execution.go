package main

import (
	"fmt"
	"strings"
	"time"
)

func testCronExecution(h *TestHarness) error {
	instanceName := testPrefix + "-cron-exec"
	port := 15436

	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("pull image for cron execution test: %w", err)
	}

	if _, err := h.createInstanceCLI(instanceName, port); err != nil {
		return fmt.Errorf("create instance for cron execution test: %w", err)
	}

	// Wait for instance to be ready
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("wait for PostgreSQL for cron execution test: %w", err)
	}

	// Debug keys are already set via KVMap before server starts
	// - cron.debug_ticker_interval.int = 1 (check every 1 second)
	// - cron.debug_force_run.int = 1 (run all plans with 100% probability)

	// List backups before creating cron schedule (should be empty)
	initialBackups, err := h.listBackupsCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list initial backups: %w", err)
	}

	// Create a cron schedule (any hour, since force run will pick it up)
	output, err := h.cronCreateBackupCLI(instanceName, 10, 7, 14)
	if err != nil {
		return fmt.Errorf("create cron schedule for execution test: %w", err)
	}
	if !strings.Contains(output, fmt.Sprintf("Scheduled daily backup for instance '%s'", instanceName)) {
		return fmt.Errorf("unexpected create output: %s", output)
	}

	// Count initial backups
	initialBackupCount := strings.Count(initialBackups, "\n")
	if initialBackupCount > 0 && strings.TrimSpace(initialBackups) == "" {
		initialBackupCount = 0
	}

	// Wait for cron to execute (with 1-second ticker and force run, it should run quickly)
	time.Sleep(5 * time.Second)

	// List backups after waiting for cron
	afterBackups, err := h.listBackupsCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list backups after cron: %w", err)
	}

	// Count backups after cron
	afterBackupCount := strings.Count(afterBackups, "\n")
	if afterBackupCount > 0 && strings.TrimSpace(afterBackups) == "" {
		afterBackupCount = 0
	}

	// Verify that a new backup was created
	if afterBackupCount <= initialBackupCount {
		return fmt.Errorf("expected new backup to be created by cron, initial count: %d, after count: %d",
			initialBackupCount, afterBackupCount)
	}

	// The backup should show "Local" location
	if !strings.Contains(afterBackups, "Local") {
		return fmt.Errorf("expected backup to show Local location, got: %s", afterBackups)
	}

	if _, err := h.cronRemoveBackupCLI(instanceName); err != nil {
		return fmt.Errorf("remove cron schedule: %w", err)
	}

	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("cleanup cron execution test instance: %w", err)
	}

	return nil
}
