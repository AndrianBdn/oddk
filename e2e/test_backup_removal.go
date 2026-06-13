package main

import (
	"fmt"
	"strconv"
	"strings"
)

func testBackupRemovalOperations(h *TestHarness) error {
	instanceName := testPrefix + "-backup-removal"
	port := 15440

	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("pull image for backup removal test: %w", err)
	}

	if _, err := h.createInstanceCLI(instanceName, port); err != nil {
		return fmt.Errorf("create instance for backup removal test: %w", err)
	}

	// Wait for instance to be ready
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("wait for PostgreSQL: %w", err)
	}

	// Test 1: Create multiple backups
	output1, err := h.runCLI("backup", "make", instanceName, "--comment", "First backup")
	if err != nil {
		return fmt.Errorf("create first backup: %w", err)
	}
	if !strings.Contains(output1, "Backup completed successfully") {
		return fmt.Errorf("unexpected first backup output: %s", output1)
	}

	output2, err := h.runCLI("backup", "make", instanceName, "--comment", "Second backup")
	if err != nil {
		return fmt.Errorf("create second backup: %w", err)
	}
	if !strings.Contains(output2, "Backup completed successfully") {
		return fmt.Errorf("unexpected second backup output: %s", output2)
	}

	output3, err := h.runCLI("backup", "make", instanceName, "--comment", "Third backup")
	if err != nil {
		return fmt.Errorf("create third backup: %w", err)
	}
	if !strings.Contains(output3, "Backup completed successfully") {
		return fmt.Errorf("unexpected third backup output: %s", output3)
	}

	// Test 2: List backups and get IDs
	listOutput, err := h.runCLI("backup", "list", "--instance", instanceName)
	if err != nil {
		return fmt.Errorf("list backups: %w", err)
	}

	// Parse backup IDs from output (looking for lines with numeric first field)
	lines := strings.Split(listOutput, "\n")
	var backupIDs []int
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			if id, err := strconv.Atoi(fields[0]); err == nil && id > 0 {
				backupIDs = append(backupIDs, id)
			}
		}
	}

	if len(backupIDs) < 3 {
		return fmt.Errorf("expected at least 3 backups, got %d", len(backupIDs))
	}

	// IDs are typically in descending order (newest first), so reverse them to match creation order
	// This ensures backupIDs[0] is the "First backup", [1] is "Second backup", etc.
	for i, j := 0, len(backupIDs)-1; i < j; i, j = i+1, j-1 {
		backupIDs[i], backupIDs[j] = backupIDs[j], backupIDs[i]
	}

	// Test 3: Remove local copy of first backup
	removeOutput, err := h.runCLI("backup", "remove-local", instanceName, strconv.Itoa(backupIDs[0]))
	if err != nil {
		return fmt.Errorf("remove local backup: %w", err)
	}
	if !strings.Contains(removeOutput, "Successfully removed local copy") {
		return fmt.Errorf("unexpected remove local output: %s", removeOutput)
	}

	// Test 4: Verify backup is removed from list (since it had no remote copy)
	listOutput2, err := h.runCLI("backup", "list", "--instance", instanceName)
	if err != nil {
		return fmt.Errorf("list backups after removal: %w", err)
	}

	// Check if the backup ID still appears in the list
	// We need to check properly formatted lines, not just any occurrence of the number
	removedID := fmt.Sprintf("%d", backupIDs[0])
	for line := range strings.SplitSeq(listOutput2, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == removedID {
			return fmt.Errorf("backup %d should have been completely removed, but still appears in list:\n%s", backupIDs[0], listOutput2)
		}
	}

	// Test 5: Try to remove non-existent backup
	_, err = h.runCLI("backup", "remove-local", instanceName, "99999")
	if err == nil {
		return fmt.Errorf("expected error when removing non-existent backup")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "backup not found") {
		return fmt.Errorf("expected 'not found' error, got: %v", err)
	}

	// Test 6: Try to remove backup from wrong instance
	wrongInstance := testPrefix + "-wrong"
	_, err = h.runCLI("backup", "remove-local", wrongInstance, strconv.Itoa(backupIDs[1]))
	if err == nil {
		return fmt.Errorf("expected error when removing backup from wrong instance")
	}
	if !strings.Contains(err.Error(), "instance not found") {
		return fmt.Errorf("expected 'instance not found' error, got: %v", err)
	}

	// Test 7: Try to remove backup with wrong instance ownership
	// First create another instance
	instance2Name := testPrefix + "-backup-removal-2"
	port2 := 15441
	if _, err := h.createInstanceCLI(instance2Name, port2); err != nil {
		return fmt.Errorf("create second instance: %w", err)
	}

	// Try to remove backup from first instance using second instance name
	_, err = h.runCLI("backup", "remove-local", instance2Name, strconv.Itoa(backupIDs[1]))
	if err == nil {
		return fmt.Errorf("expected error when removing backup owned by different instance")
	}
	if !strings.Contains(err.Error(), "does not belong to instance") {
		return fmt.Errorf("expected ownership error, got: %v", err)
	}

	// Test 8: Remove another local backup (should work)
	removeOutput2, err := h.runCLI("backup", "remove-local", instanceName, strconv.Itoa(backupIDs[1]))
	if err != nil {
		return fmt.Errorf("remove second local backup: %w", err)
	}
	if !strings.Contains(removeOutput2, "Successfully removed local copy") {
		return fmt.Errorf("unexpected second remove output: %s", removeOutput2)
	}

	// Test 9: Verify only one backup remains
	listOutput3, err := h.runCLI("backup", "list", "--instance", instanceName)
	if err != nil {
		return fmt.Errorf("list backups after second removal: %w", err)
	}

	remainingBackups := 0
	for _, id := range backupIDs {
		idStr := fmt.Sprintf("%d", id)
		// Check if ID appears as first field in any line
		for line := range strings.SplitSeq(listOutput3, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[0] == idStr {
				remainingBackups++
				break
			}
		}
	}

	if remainingBackups != 1 {
		return fmt.Errorf("expected 1 remaining backup, got %d", remainingBackups)
	}

	// Test 10: Try to remove remote copy when there is none (backup only has local copy)
	_, err = h.runCLI("backup", "remove-remote", instanceName, strconv.Itoa(backupIDs[2]))
	if err == nil {
		return fmt.Errorf("expected error when removing non-existent remote copy")
	}
	if !strings.Contains(err.Error(), "no remote copy") {
		return fmt.Errorf("expected 'no remote copy' error, got: %v", err)
	}

	if err := h.destroyInstanceCLI(instance2Name); err != nil {
		return fmt.Errorf("cleanup second instance: %w", err)
	}

	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("cleanup main instance: %w", err)
	}

	return nil
}
