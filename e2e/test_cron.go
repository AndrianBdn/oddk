package main

import (
	"fmt"
	"strconv"
	"strings"
)

func testCronCRUD(h *TestHarness) error {
	instanceName := testPrefix + "-cron"
	port := 15433

	// Setup: Create an instance first (cron requires instance to exist)
	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("pull image for cron test: %w", err)
	}

	if _, err := h.createInstanceCLI(instanceName, port); err != nil {
		return fmt.Errorf("create instance for cron test: %w", err)
	}

	// Wait for instance to be ready
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("wait for PostgreSQL for cron test: %w", err)
	}

	// Test 1: List cron schedules (should be empty initially)
	output, err := h.cronListCLI()
	if err != nil {
		return fmt.Errorf("initial cron list: %w", err)
	}
	// The output could be either the message or an empty result - both are acceptable initially
	if !strings.Contains(output, "No scheduled backups configured") && strings.TrimSpace(output) != "" {
		return fmt.Errorf("expected no scheduled backups message or empty output, got: %s", output)
	}

	// Test 2: Create cron schedule
	output, err = h.cronCreateBackupCLI(instanceName, 3, 7, 14)
	if err != nil {
		return fmt.Errorf("create cron schedule: %w", err)
	}
	expectedMsg := fmt.Sprintf("Scheduled daily backup for instance '%s' at 03:00 UTC (keep local: 7 days, remote: 14 days)", instanceName)
	if !strings.Contains(output, expectedMsg) {
		return fmt.Errorf("create output should contain '%s', got: '%s'", expectedMsg, output)
	}

	// Test 3: List cron schedules (should show our schedule)
	output, err = h.cronListCLI()
	if err != nil {
		return fmt.Errorf("cron list after create: %w", err)
	}
	if !strings.Contains(output, instanceName) {
		return fmt.Errorf("cron list should contain instance name, got: %s", output)
	}
	if !strings.Contains(output, "3") {
		return fmt.Errorf("cron list should contain UTC hour 3, got: %s", output)
	}
	if !strings.Contains(output, "03:00 UTC") {
		return fmt.Errorf("cron list should contain schedule time, got: %s", output)
	}
	if !strings.Contains(output, "7 days") || !strings.Contains(output, "14 days") {
		return fmt.Errorf("cron list should show cleanup days, got: %s", output)
	}

	// Test 4: Update cron schedule (creating with different hour and cleanup days should replace)
	output, err = h.cronCreateBackupCLI(instanceName, 15, 10, 20)
	if err != nil {
		return fmt.Errorf("update cron schedule: %w", err)
	}
	if !strings.Contains(output, fmt.Sprintf("Scheduled daily backup for instance '%s' at 15:00 UTC (keep local: 10 days, remote: 20 days)", instanceName)) {
		return fmt.Errorf("unexpected update output: %s", output)
	}

	// Test 5: Verify schedule was updated
	output, err = h.cronListCLI()
	if err != nil {
		return fmt.Errorf("cron list after update: %w", err)
	}
	if !strings.Contains(output, "15") {
		return fmt.Errorf("cron list should contain updated UTC hour 15, got: %s", output)
	}
	if !strings.Contains(output, "15:00 UTC") {
		return fmt.Errorf("cron list should contain updated schedule time, got: %s", output)
	}
	if strings.Contains(output, "03:00 UTC") {
		return fmt.Errorf("cron list should not contain old schedule time, got: %s", output)
	}

	// Test 6: Remove cron schedule
	output, err = h.cronRemoveBackupCLI(instanceName)
	if err != nil {
		return fmt.Errorf("remove cron schedule: %w", err)
	}
	if !strings.Contains(output, fmt.Sprintf("Removed scheduled backup for instance '%s'", instanceName)) {
		return fmt.Errorf("unexpected remove output: %s", output)
	}

	// Test 7: Verify schedule was removed
	output, err = h.cronListCLI()
	if err != nil {
		return fmt.Errorf("cron list after remove: %w", err)
	}
	// After removal, we should either see the "no backups" message or an empty result
	if !strings.Contains(output, "No scheduled backups configured") && strings.TrimSpace(output) != "" {
		return fmt.Errorf("expected no scheduled backups after remove, got: %s", output)
	}

	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("cleanup cron test instance: %w", err)
	}

	return nil
}

func testCronValidation(h *TestHarness) error {
	instanceName := testPrefix + "-cron-validation"

	// Test 1: Try to create cron schedule for non-existent instance
	_, err := h.cronCreateBackupCLI(instanceName, 12, 7, 14)
	if err == nil {
		return fmt.Errorf("expected error when creating cron for non-existent instance")
	}
	if !strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("expected 'not found' error, got: %v", err)
	}

	// Test 2: Try to remove cron schedule for non-existent instance
	_, err = h.cronRemoveBackupCLI(instanceName)
	if err == nil {
		return fmt.Errorf("expected error when removing cron for non-existent instance")
	}

	// Test 3: Test UTC hour validation (invalid hours)
	invalidHours := []int{-1, 24, 25, 100}
	for _, hour := range invalidHours {
		_, err := h.cronCreateBackupCLI("dummy", hour, 7, 14)
		if err == nil {
			return fmt.Errorf("expected error for invalid UTC hour %d", hour)
		}
		if !strings.Contains(err.Error(), "UTC hour must be between 0 and 23") {
			return fmt.Errorf("expected UTC hour validation error for %d, got: %v", hour, err)
		}
	}

	// Test 4: Test UTC hour validation (valid hours)
	// We'll just test a few valid hours without actually creating instances
	// This tests the CLI validation before it hits the API
	validHours := []int{0, 1, 12, 23}
	for _, hour := range validHours {
		// This should fail with instance not found, not hour validation
		_, err := h.cronCreateBackupCLI("nonexistent", hour, 7, 14)
		if err == nil {
			return fmt.Errorf("expected instance not found error for hour %d", hour)
		}
		if strings.Contains(err.Error(), "UTC hour must be between 0 and 23") {
			return fmt.Errorf("should not get hour validation error for valid hour %d", hour)
		}
	}

	return nil
}

func testCronMultipleInstances(h *TestHarness) error {
	instance1 := testPrefix + "-cron-multi1"
	instance2 := testPrefix + "-cron-multi2"
	port1 := 15434
	port2 := 15435

	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("pull image for multi-instance cron test: %w", err)
	}

	if _, err := h.createInstanceCLI(instance1, port1); err != nil {
		return fmt.Errorf("create instance1 for cron test: %w", err)
	}

	if _, err := h.createInstanceCLI(instance2, port2); err != nil {
		return fmt.Errorf("create instance2 for cron test: %w", err)
	}

	// Wait for instances to be ready
	if err := h.waitForPostgreSQL(port1); err != nil {
		return fmt.Errorf("wait for PostgreSQL instance1: %w", err)
	}
	if err := h.waitForPostgreSQL(port2); err != nil {
		return fmt.Errorf("wait for PostgreSQL instance2: %w", err)
	}

	// Test 1: Create cron schedules for both instances
	if _, err := h.cronCreateBackupCLI(instance1, 2, 7, 14); err != nil {
		return fmt.Errorf("create cron for instance1: %w", err)
	}

	if _, err := h.cronCreateBackupCLI(instance2, 14, 5, 30); err != nil {
		return fmt.Errorf("create cron for instance2: %w", err)
	}

	// Test 2: List should show both schedules
	output, err := h.cronListCLI()
	if err != nil {
		return fmt.Errorf("list crons for multiple instances: %w", err)
	}

	if !strings.Contains(output, instance1) {
		return fmt.Errorf("list should contain instance1 '%s', got: '%s'", instance1, output)
	}
	if !strings.Contains(output, instance2) {
		return fmt.Errorf("list should contain instance2 '%s', got: '%s'", instance2, output)
	}
	if !strings.Contains(output, "02:00 UTC") {
		return fmt.Errorf("list should contain instance1 schedule '02:00 UTC', got: '%s'", output)
	}
	if !strings.Contains(output, "14:00 UTC") {
		return fmt.Errorf("list should contain instance2 schedule '14:00 UTC', got: '%s'", output)
	}

	// Test 3: Remove one schedule
	if _, err := h.cronRemoveBackupCLI(instance1); err != nil {
		return fmt.Errorf("remove cron for instance1: %w", err)
	}

	// Test 4: List should show only instance2 schedule
	output, err = h.cronListCLI()
	if err != nil {
		return fmt.Errorf("list crons after removing one: %w", err)
	}

	if strings.Contains(output, instance1) {
		return fmt.Errorf("list should not contain instance1 after removal, got: %s", output)
	}
	if !strings.Contains(output, instance2) {
		return fmt.Errorf("list should still contain instance2, got: %s", output)
	}
	if !strings.Contains(output, "14:00 UTC") {
		return fmt.Errorf("list should still contain instance2 schedule, got: %s", output)
	}

	// Test 5: Remove remaining schedule
	if _, err := h.cronRemoveBackupCLI(instance2); err != nil {
		return fmt.Errorf("remove cron for instance2: %w", err)
	}

	// Test 6: List should be empty
	output, err = h.cronListCLI()
	if err != nil {
		return fmt.Errorf("list crons after removing all: %w", err)
	}

	// After removing all, we should either see the "no backups" message or an empty result
	if !strings.Contains(output, "No scheduled backups configured") && strings.TrimSpace(output) != "" {
		return fmt.Errorf("expected no schedules after removing all, got: %s", output)
	}

	if err := h.destroyInstanceCLI(instance1); err != nil {
		return fmt.Errorf("cleanup instance1: %w", err)
	}
	if err := h.destroyInstanceCLI(instance2); err != nil {
		return fmt.Errorf("cleanup instance2: %w", err)
	}

	return nil
}

// CLI helper methods for cron operations
func (h *TestHarness) cronCreateBackupCLI(instanceName string, utcHour, cleanupLocalDays, cleanupRemoteDays int) (string, error) {
	return h.runCLI("backup", "setup-cron", "--instance", instanceName, "--utc-hour", strconv.Itoa(utcHour),
		"--cleanup-local-days", strconv.Itoa(cleanupLocalDays),
		"--cleanup-remote-days", strconv.Itoa(cleanupRemoteDays))
}

func (h *TestHarness) cronRemoveBackupCLI(instanceName string) (string, error) {
	return h.runCLI("backup", "setup-cron", "--instance", instanceName, "--remove")
}

func (h *TestHarness) cronListCLI() (string, error) {
	return h.runCLI("backup", "list-cron")
}

func testCronCleanupDays(h *TestHarness) error {
	instanceName := testPrefix + "-cron-cleanup"
	port := 15436

	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("pull image for cleanup days test: %w", err)
	}

	if _, err := h.createInstanceCLI(instanceName, port); err != nil {
		return fmt.Errorf("create instance for cleanup days test: %w", err)
	}

	// Wait for instance to be ready
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("wait for PostgreSQL for cleanup days test: %w", err)
	}

	// Test 1: Create with custom cleanup days
	output, err := h.cronCreateBackupCLI(instanceName, 5, 30, 60)
	if err != nil {
		return fmt.Errorf("create cron with custom cleanup days: %w", err)
	}
	expectedMsg := fmt.Sprintf("Scheduled daily backup for instance '%s' at 05:00 UTC (keep local: 30 days, remote: 60 days)", instanceName)
	if !strings.Contains(output, expectedMsg) {
		return fmt.Errorf("create output should contain '%s', got: '%s'", expectedMsg, output)
	}

	// Test 2: Verify list shows custom cleanup days
	output, err = h.cronListCLI()
	if err != nil {
		return fmt.Errorf("list cron after creating with custom days: %w", err)
	}
	if !strings.Contains(output, "30 days") || !strings.Contains(output, "60 days") {
		return fmt.Errorf("list should show custom cleanup days (30 and 60), got: %s", output)
	}

	// Test 3: Test minimum cleanup days validation (using direct CLI call to test client-side validation)
	_, err = h.runCLI("backup", "setup-cron", "--instance", instanceName, "--utc-hour", "6",
		"--cleanup-local-days", "0", "--cleanup-remote-days", "14")
	if err == nil {
		return fmt.Errorf("expected error for cleanup-local-days = 0")
	}
	if !strings.Contains(err.Error(), "cleanup-local-days must be at least 1") {
		return fmt.Errorf("expected validation error for cleanup-local-days = 0, got: %v", err)
	}

	_, err = h.runCLI("backup", "setup-cron", "--instance", instanceName, "--utc-hour", "6",
		"--cleanup-local-days", "7", "--cleanup-remote-days", "0")
	if err == nil {
		return fmt.Errorf("expected error for cleanup-remote-days = 0")
	}
	if !strings.Contains(err.Error(), "cleanup-remote-days must be at least 1") {
		return fmt.Errorf("expected validation error for cleanup-remote-days = 0, got: %v", err)
	}

	// Test 4: Update with different cleanup days
	output, err = h.cronCreateBackupCLI(instanceName, 10, 3, 7)
	if err != nil {
		return fmt.Errorf("update cron with different cleanup days: %w", err)
	}
	expectedMsg = fmt.Sprintf("Scheduled daily backup for instance '%s' at 10:00 UTC (keep local: 3 days, remote: 7 days)", instanceName)
	if !strings.Contains(output, expectedMsg) {
		return fmt.Errorf("update output should contain '%s', got: '%s'", expectedMsg, output)
	}

	// Test 5: Verify list shows updated cleanup days
	output, err = h.cronListCLI()
	if err != nil {
		return fmt.Errorf("list cron after update: %w", err)
	}
	if !strings.Contains(output, "3 days") || !strings.Contains(output, "7 days") {
		return fmt.Errorf("list should show updated cleanup days (3 and 7), got: %s", output)
	}
	// Make sure old values are not present
	if strings.Contains(output, "30 days") || strings.Contains(output, "60 days") {
		return fmt.Errorf("list should not show old cleanup days (30 and 60), got: %s", output)
	}

	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("cleanup instance: %w", err)
	}

	return nil
}
