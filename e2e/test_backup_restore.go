package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

func testBackupRestore(h *TestHarness) error {
	log.Println("=== Testing Backup Restore ===")

	log.Println("Step 1: Pulling PostgreSQL 17 image")
	_, err := h.pullImageCLI("17")
	if err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	instanceName := fmt.Sprintf("oddk-danger-funct-restore-%d", time.Now().Unix())
	port := 15445

	log.Println("Step 1: Creating test instance")
	output, err := h.runCLI("create",
		"--name", instanceName,
		"--version", "17",
		"--port", strconv.Itoa(port),
		"--cpu", "1",
		"--ram", "512M")
	if err != nil {
		return fmt.Errorf("failed to create instance: %w (output: %s)", err, output)
	}

	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	log.Println("Step 2: Creating test database")
	output, err = h.runCLI("instance", "create-db", instanceName, "--database", "testdb")
	if err != nil {
		return fmt.Errorf("failed to create database: %w (output: %s)", err, output)
	}

	log.Println("Step 3: Creating backup")
	output, err = h.runCLI("backup", "make", instanceName)
	if err != nil {
		return fmt.Errorf("failed to create backup: %w (output: %s)", err, output)
	}

	log.Println("Step 4: Getting backup ID")
	output, err = h.runCLI("backup", "list", "--instance", instanceName)
	if err != nil {
		return fmt.Errorf("failed to list backups: %w (output: %s)", err, output)
	}

	lines := strings.Split(output, "\n")
	var backupID int
	for i, line := range lines {
		if i == 0 || line == "" {
			continue // Skip header
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			backupID, err = strconv.Atoi(fields[0])
			if err == nil {
				break
			}
		}
	}
	if backupID == 0 {
		return fmt.Errorf("could not find backup ID in output: %s", output)
	}
	log.Printf("Found backup ID: %d", backupID)

	log.Println("Step 5: Restoring database to new name (testdb_restored)")
	output, err = h.runCLI("backup", "restore",
		"--instance", instanceName,
		"--id", strconv.Itoa(backupID),
		"--database", "testdb",
		"--restore-as", "testdb_restored")
	if err != nil {
		return fmt.Errorf("failed to restore: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Successfully restored") {
		return fmt.Errorf("expected success message, got: %s", output)
	}
	if !strings.Contains(output, "testdb_restored") {
		return fmt.Errorf("expected target database name in output, got: %s", output)
	}

	log.Println("Step 6: Verifying restored database exists")
	output, err = h.runCLI("instance", "list-dbs", instanceName)
	if err != nil {
		return fmt.Errorf("failed to list databases: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "testdb_restored") {
		return fmt.Errorf("restored database not found in list: %s", output)
	}
	// Original should still exist
	if !strings.Contains(output, "testdb") {
		return fmt.Errorf("original database not found in list: %s", output)
	}

	log.Println("Step 7: Testing restore fails for existing database")
	output, err = h.runCLI("backup", "restore",
		"--instance", instanceName,
		"--id", strconv.Itoa(backupID),
		"--database", "testdb",
		"--restore-as", "testdb_restored")
	if err == nil {
		return fmt.Errorf("expected restore to fail for existing database, output: %s", output)
	}
	if !strings.Contains(err.Error(), "already exists") && !strings.Contains(output, "already exists") {
		return fmt.Errorf("expected 'already exists' error, got: %v (output: %s)", err, output)
	}

	log.Println("Step 8: Testing restore with non-existent database in backup")
	output, err = h.runCLI("backup", "restore",
		"--instance", instanceName,
		"--id", strconv.Itoa(backupID),
		"--database", "nonexistent_db")
	if err == nil {
		return fmt.Errorf("expected restore to fail for non-existent database in backup, output: %s", output)
	}
	if !strings.Contains(err.Error(), "not found in backup") && !strings.Contains(output, "not found in backup") {
		return fmt.Errorf("expected 'not found in backup' error, got: %v (output: %s)", err, output)
	}

	log.Println("Step 9: Restoring database with same name to new database")
	output, err = h.runCLI("backup", "restore",
		"--instance", instanceName,
		"--id", strconv.Itoa(backupID),
		"--database", "testdb",
		"--restore-as", "testdb_copy")
	if err != nil {
		return fmt.Errorf("failed to restore with same name: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Successfully restored") {
		return fmt.Errorf("expected success message, got: %s", output)
	}

	log.Println("Step 10: Verifying all databases exist")
	output, err = h.runCLI("instance", "list-dbs", instanceName)
	if err != nil {
		return fmt.Errorf("failed to list databases: %w (output: %s)", err, output)
	}
	for _, dbName := range []string{"testdb", "testdb_restored", "testdb_copy"} {
		if !strings.Contains(output, dbName) {
			return fmt.Errorf("database %s not found in list: %s", dbName, output)
		}
	}

	log.Println("Step 11: Testing restore with invalid backup ID")
	output, err = h.runCLI("backup", "restore",
		"--instance", instanceName,
		"--id", "99999",
		"--database", "testdb")
	if err == nil {
		return fmt.Errorf("expected restore to fail with invalid backup ID, output: %s", output)
	}
	if !strings.Contains(err.Error(), "backup not found") && !strings.Contains(output, "backup not found") {
		return fmt.Errorf("expected 'backup not found' error, got: %v (output: %s)", err, output)
	}

	log.Println("Step 12: Cleaning up")
	_, err = h.runCLI("instance", "destroy", instanceName, "--force")
	if err != nil {
		return fmt.Errorf("failed to destroy instance: %w", err)
	}

	log.Println("=== Backup Restore Test PASSED ===")
	return nil
}
