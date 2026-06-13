package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

func testFullLifecycle(h *TestHarness) error {
	instanceName := testPrefix + "-lifecycle"
	port := 15432

	// Test 0: Pull image first
	output, err := h.pullImageCLI("16")
	if err != nil {
		return fmt.Errorf("pull image via CLI: %w", err)
	}
	if !strings.Contains(output, "postgres:16") {
		return fmt.Errorf("pull output should mention postgres:16")
	}

	// Test 1: Create instance using CLI
	if _, err := h.createInstanceCLI(instanceName, port); err != nil {
		return fmt.Errorf("create instance via CLI: %w", err)
	}

	// Wait for container to be ready
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	// Test 2: Get instance status using CLI
	if _, err := h.getInstanceStatusCLI(instanceName); err != nil {
		return fmt.Errorf("get instance status via CLI: %w", err)
	}

	// Test 3: List instances using CLI
	if _, err := h.listInstancesCLI(); err != nil {
		return fmt.Errorf("list instances via CLI: %w", err)
	}

	// Test 4: Stop instance using CLI
	if _, err := h.stopInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("stop instance via CLI: %w", err)
	}

	// Small delay for container to fully stop
	time.Sleep(500 * time.Millisecond)

	// Verify it's stopped using CLI
	if _, err := h.getInstanceStatusCLI(instanceName); err != nil {
		return fmt.Errorf("get stopped instance status via CLI: %w", err)
	}

	// Test 5: Start instance using CLI
	if _, err := h.startInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("start instance via CLI: %w", err)
	}

	// Wait for container to start
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready after restart: %w", err)
	}

	// Verify it's running using CLI
	if _, err := h.getInstanceStatusCLI(instanceName); err != nil {
		return fmt.Errorf("get restarted instance status via CLI: %w", err)
	}

	// Test 6: List databases using CLI
	output, err = h.listDatabasesCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list databases via CLI: %w", err)
	}
	// Should at least have the default postgres database
	if !strings.Contains(output, "postgres") {
		return fmt.Errorf("list databases output should contain 'postgres' database: %s", output)
	}

	// Test 7: Destroy instance (using API to avoid interactive prompt)
	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("destroy instance: %w", err)
	}

	// Verify it's gone using API
	deleteStatus, _, reqErr := h.request("GET", "/api/rdbms/"+instanceName, nil)
	if reqErr != nil {
		return fmt.Errorf("get deleted instance request failed: %w", reqErr)
	}

	if deleteStatus != http.StatusNotFound {
		return fmt.Errorf("expected status 404 for deleted instance, got %d", deleteStatus)
	}

	return nil
}

func testConsistencyChecks(h *TestHarness) error {
	instanceName := testPrefix + "-consistency"
	port := 15433

	// Pull image first (may already exist from previous test, that's okay)
	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("pull image via CLI: %w", err)
	}

	if _, err := h.createInstanceCLI(instanceName, port); err != nil {
		return fmt.Errorf("create instance via CLI: %w", err)
	}

	// Wait for container to be ready
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	// Manually stop the container to simulate inconsistency
	ctx := context.Background()
	containerName := "oddk-pg-" + instanceName
	if err := h.docker.ContainerStop(ctx, containerName, container.StopOptions{}); err != nil {
		return fmt.Errorf("failed to stop container manually: %w", err)
	}

	// Small delay for container to fully stop
	time.Sleep(500 * time.Millisecond)

	// Get instance status using CLI - consistency check should detect stopped container
	if _, err := h.getInstanceStatusCLI(instanceName); err != nil {
		return fmt.Errorf("get instance status via CLI after manual stop: %w", err)
	}

	// Clean up using API (to avoid confirmation prompt)
	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("cleanup failed: %w", err)
	}

	return nil
}

func testMultipleInstances(h *TestHarness) error {
	instances := []struct {
		name string
		port int
	}{
		{testPrefix + "-multi1", 15434},
		{testPrefix + "-multi2", 15435},
		{testPrefix + "-multi3", 15436},
	}

	// Pull image first (may already exist from previous tests, that's okay)
	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("pull image via CLI: %w", err)
	}

	for _, inst := range instances {
		output, err := h.createInstanceCLI(inst.name, inst.port)
		if err != nil {
			return fmt.Errorf("create instance %s via CLI: %w", inst.name, err)
		}
		// Verify output contains instance name
		if !strings.Contains(output, inst.name) {
			return fmt.Errorf("create output should contain instance name %s, got: %s", inst.name, output)
		}
	}

	// Wait for all containers to be ready
	for _, inst := range instances {
		if err := h.waitForPostgreSQL(inst.port); err != nil {
			return fmt.Errorf("PostgreSQL %s not ready: %w", inst.name, err)
		}
	}

	// List all instances using CLI and verify output
	listOutput, err := h.listInstancesCLI()
	if err != nil {
		return fmt.Errorf("list instances via CLI: %w", err)
	}

	// Verify all instances appear in the list
	for _, inst := range instances {
		if !strings.Contains(listOutput, inst.name) {
			return fmt.Errorf("list output should contain instance %s, got: %s", inst.name, listOutput)
		}
		if !strings.Contains(listOutput, fmt.Sprintf("%d", inst.port)) {
			return fmt.Errorf("list output should contain port %d, got: %s", inst.port, listOutput)
		}
	}

	for _, inst := range instances {
		statusOutput, err := h.getInstanceStatusCLI(inst.name)
		if err != nil {
			return fmt.Errorf("get status of %s via CLI: %w", inst.name, err)
		}
		// Verify status output contains instance info
		if !strings.Contains(statusOutput, inst.name) {
			return fmt.Errorf("status output should contain instance name %s, got: %s", inst.name, statusOutput)
		}
		if !strings.Contains(statusOutput, "running") {
			return fmt.Errorf("status output should show running status for %s, got: %s", inst.name, statusOutput)
		}
	}

	// Clean up all instances using API (to avoid confirmation prompts)
	for _, inst := range instances {
		if err := h.destroyInstanceCLI(inst.name); err != nil {
			return fmt.Errorf("cleanup instance %s: %w", inst.name, err)
		}
	}

	return nil
}

func testCLIDirectUsage(h *TestHarness) error {
	instanceName := testPrefix + "-cli-test"
	port := 15437

	// Test 0: Pull image first using harness environment (like other tests)
	_, err := h.pullImageCLI("16")
	if err != nil {
		return fmt.Errorf("cli pull failed: %w", err)
	}

	// Test 1: Create instance using CLI with harness environment
	_, err = h.createInstanceCLI(instanceName, port)
	if err != nil {
		return fmt.Errorf("cli create failed: %w", err)
	}

	// Wait for container to be ready
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	// Test 2: List instances using CLI
	_, err = h.listInstancesCLI()
	if err != nil {
		return fmt.Errorf("cli list failed: %w", err)
	}

	// Test 3: Get status using CLI
	_, err = h.getInstanceStatusCLI(instanceName)
	if err != nil {
		return fmt.Errorf("cli status failed: %w", err)
	}

	// Test 4: Stop instance using CLI
	_, err = h.stopInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("cli stop failed: %w", err)
	}

	// Small delay for container to fully stop
	time.Sleep(500 * time.Millisecond)

	// Test 5: Start instance using CLI
	_, err = h.startInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("cli start failed: %w", err)
	}

	// Wait for container to start
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready after restart: %w", err)
	}

	// Test 6: Create a second instance using harness environment (like other tests)
	secondInstance := testPrefix + "-cli-test2"
	_, err = h.createInstanceCLI(secondInstance, 15438)
	if err != nil {
		return fmt.Errorf("cli create second instance failed: %w", err)
	}

	// Clean up both instances via API (not CLI to avoid confirmation prompt)
	if err := h.deleteInstance(instanceName); err != nil {
		return fmt.Errorf("cleanup first instance: %w", err)
	}

	if err := h.deleteInstance(secondInstance); err != nil {
		return fmt.Errorf("cleanup second instance: %w", err)
	}

	return nil
}

func testBackupOperations(h *TestHarness) error {
	instanceName := testPrefix + "-backup"
	port := 15440

	// Pull image first
	output, err := h.pullImageCLI("16")
	if err != nil {
		return fmt.Errorf("pull failed: %w", err)
	}
	if !strings.Contains(output, "postgres:16") {
		return fmt.Errorf("pull output should mention postgres:16")
	}

	createOutput, err := h.createInstanceCLI(instanceName, port)
	if err != nil {
		return fmt.Errorf("create failed: %w", err)
	}
	if !strings.Contains(createOutput, instanceName) {
		return fmt.Errorf("create output should contain instance name: %s", createOutput)
	}

	// Wait for container to be ready (PostgreSQL takes time to initialize)
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	// Verify instance is running before backup
	statusOutput, err := h.getInstanceStatusCLI(instanceName)
	if err != nil {
		return fmt.Errorf("status check failed: %w", err)
	}
	if !strings.Contains(statusOutput, "running") {
		return fmt.Errorf("instance not running before backup: %s", statusOutput)
	}

	backupOutput, err := h.backupInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}
	if !strings.Contains(backupOutput, "Backup completed") && !strings.Contains(backupOutput, "successfully") {
		return fmt.Errorf("backup output should indicate success: %s", backupOutput)
	}

	// List backups - should have 1
	listOutput, err := h.listBackupsCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list-backups failed: %w", err)
	}
	if !strings.Contains(listOutput, "Local") {
		return fmt.Errorf("list-backups should show backup with Local location: %s", listOutput)
	}

	backup2Output, err := h.backupInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("second backup failed: %w", err)
	}
	if !strings.Contains(backup2Output, "Backup completed") && !strings.Contains(backup2Output, "successfully") {
		return fmt.Errorf("second backup output should indicate success: %s", backup2Output)
	}

	// List backups again - should have 2
	list2Output, err := h.listBackupsCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list-backups after second backup failed: %w", err)
	}

	// Count backups in output - look for rows with "Local" location
	lines := strings.Split(list2Output, "\n")
	backupCount := 0
	for _, line := range lines {
		if strings.Contains(line, "Local") && strings.Contains(line, "completed") {
			backupCount++
		}
	}
	if backupCount < 2 {
		return fmt.Errorf("expected at least 2 backups, found %d in output: %s", backupCount, list2Output)
	}

	if err := h.deleteInstance(instanceName); err != nil {
		return fmt.Errorf("cleanup instance: %w", err)
	}

	return nil
}

// testDatabaseManagement tests database and user management operations
func testDatabaseManagement(h *TestHarness) error {
	instanceName := testPrefix + "-dbmgmt"
	port := 15444
	databaseName := "testapp"
	rwUsername := "appuser"
	roUsername := "readonlyuser"

	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("pull image: %w", err)
	}

	if _, err := h.createInstanceCLI(instanceName, port); err != nil {
		return fmt.Errorf("create instance: %w", err)
	}

	// Wait for PostgreSQL to be ready
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	// Test 1: Create database
	createOutput, err := h.createDatabaseCLI(instanceName, databaseName)
	if err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	if !strings.Contains(createOutput, "created successfully") {
		return fmt.Errorf("create database output should indicate success: %s", createOutput)
	}

	// Test 2: Verify database appears in list-dbs
	listOutput, err := h.listDatabasesCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list databases: %w", err)
	}
	if !strings.Contains(listOutput, databaseName) {
		return fmt.Errorf("list databases should contain %s: %s", databaseName, listOutput)
	}

	// Test 3: Add read-write user
	addRwOutput, err := h.addDatabaseUserCLI(instanceName, rwUsername, databaseName, false)
	if err != nil {
		return fmt.Errorf("add read-write user: %w", err)
	}
	if !strings.Contains(addRwOutput, "created successfully") {
		return fmt.Errorf("add user output should indicate success: %s", addRwOutput)
	}
	if !strings.Contains(addRwOutput, "read-write") {
		return fmt.Errorf("add user output should indicate read-write access: %s", addRwOutput)
	}
	if !strings.Contains(addRwOutput, "Password:") {
		return fmt.Errorf("add user output should contain password: %s", addRwOutput)
	}

	// Test 4: Add read-only user
	addRoOutput, err := h.addDatabaseUserCLI(instanceName, roUsername, databaseName, true)
	if err != nil {
		return fmt.Errorf("add read-only user: %w", err)
	}
	if !strings.Contains(addRoOutput, "created successfully") {
		return fmt.Errorf("add read-only user output should indicate success: %s", addRoOutput)
	}
	if !strings.Contains(addRoOutput, "read-only") {
		return fmt.Errorf("add user output should indicate read-only access: %s", addRoOutput)
	}

	// Test 5: Reset user password
	resetOutput, err := h.resetDatabaseUserPasswordCLI(instanceName, rwUsername)
	if err != nil {
		return fmt.Errorf("reset user password: %w", err)
	}
	if !strings.Contains(resetOutput, "reset successfully") {
		return fmt.Errorf("reset password output should indicate success: %s", resetOutput)
	}
	if !strings.Contains(resetOutput, "Password:") {
		return fmt.Errorf("reset password output should contain new password: %s", resetOutput)
	}

	// Test 6: Try to create duplicate database (should fail)
	_, err = h.createDatabaseCLI(instanceName, databaseName)
	if err == nil {
		return fmt.Errorf("creating duplicate database should fail")
	}
	if !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("duplicate database error should mention 'already exists': %v", err)
	}

	// Test 7: Try to create duplicate user (should fail)
	_, err = h.addDatabaseUserCLI(instanceName, rwUsername, databaseName, false)
	if err == nil {
		return fmt.Errorf("creating duplicate user should fail")
	}
	if !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("duplicate user error should mention 'already exists': %v", err)
	}

	// Test 8: Try to create user for non-existent database (should fail)
	_, err = h.addDatabaseUserCLI(instanceName, "baduser", "nonexistent", false)
	if err == nil {
		return fmt.Errorf("creating user for non-existent database should fail")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		return fmt.Errorf("non-existent database error should mention 'does not exist': %v", err)
	}

	// Test 9: Delete read-only user
	deleteOutput, err := h.deleteDatabaseUserCLI(instanceName, roUsername)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if !strings.Contains(deleteOutput, "deleted successfully") {
		return fmt.Errorf("delete user output should indicate success: %s", deleteOutput)
	}

	// Test 10: Try to reset password for non-existent user (should fail)
	_, err = h.resetDatabaseUserPasswordCLI(instanceName, roUsername)
	if err == nil {
		return fmt.Errorf("resetting password for deleted user should fail")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		return fmt.Errorf("non-existent user error should mention 'does not exist': %v", err)
	}

	// Test 11: Try to delete non-existent user (should fail)
	_, err = h.deleteDatabaseUserCLI(instanceName, "nonexistentuser")
	if err == nil {
		return fmt.Errorf("deleting non-existent user should fail")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		return fmt.Errorf("non-existent user error should mention 'does not exist': %v", err)
	}

	if err := h.deleteInstance(instanceName); err != nil {
		return fmt.Errorf("cleanup instance: %w", err)
	}

	return nil
}
