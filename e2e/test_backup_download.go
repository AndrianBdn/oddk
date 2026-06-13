package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func testBackupDownloadFromS3(h *TestHarness) error {
	log.Println("=== Testing Backup Download from S3 ===")

	log.Println("Step 1: Pulling PostgreSQL 17 image")
	_, err := h.pullImageCLI("17")
	if err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	log.Println("Step 1: Creating test instance")
	instanceName := fmt.Sprintf("oddk-danger-funct-backup-download-%d", time.Now().Unix())

	output, err := h.runCLI("create",
		"--name", instanceName,
		"--version", "17",
		"--port", "15433",
		"--cpu", "1",
		"--ram", "512M")
	if err != nil {
		return fmt.Errorf("failed to create instance: %w", err)
	}
	if !strings.Contains(output, "Created RDBMS instance") {
		return fmt.Errorf("expected create output to contain 'Created RDBMS instance', got: %q", output)
	}

	// Instance is created in running state, so just wait for PostgreSQL to be ready
	log.Println("Step 2: Waiting for PostgreSQL to be ready")
	if err := h.waitForPostgreSQL(15433); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	log.Println("Step 3: Configuring offsite backup")
	log.Printf("FakeS3 URL: %s", h.fakeS3URL)
	config := map[string]any{
		"type":            "s3",
		"bucket":          "test-backup-download",
		"endpoint":        h.fakeS3URL,
		"accessKeyId":     "test-key",
		"secretAccessKey": "test-secret",
		"bucketPath":      "downloads/",
		"region":          "us-east-1",
	}

	configFile := filepath.Join(h.dataDir, "offsite-config.json")
	configData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	if err := os.WriteFile(configFile, configData, 0o600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	output, err = h.runCLI("offsite", "apply", "--file", configFile)
	if err != nil {
		return fmt.Errorf("failed to apply offsite config: %w", err)
	}
	if !strings.Contains(output, "Offsite configuration applied successfully") {
		return fmt.Errorf("expected apply output to contain 'Offsite configuration applied successfully', got: %q", output)
	}

	log.Println("Step 4: Creating backup")
	output, err = h.runCLI("backup", "make", instanceName, "--comment", "Test backup for download")
	if err != nil {
		return fmt.Errorf("failed to create backup: %w", err)
	}
	if !strings.Contains(output, "Backup completed successfully") {
		return fmt.Errorf("expected backup output to contain 'Backup completed successfully', got: %q", output)
	}

	log.Println("Step 5: Getting backup ID")
	output, err = h.runCLI("backup", "list", "--instance", instanceName)
	if err != nil {
		return fmt.Errorf("failed to list backups: %w", err)
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
		return fmt.Errorf("could not find backup ID in list output, got: %q", output)
	}
	log.Printf("Found backup ID: %d", backupID)

	// Verify backup shows "Local" location initially
	if !strings.Contains(output, "Local") {
		return fmt.Errorf("expected backup to show Local location initially, got: %q", output)
	}

	log.Println("Step 6: Uploading backup to S3")
	output, err = h.runCLI("backup", "upload", instanceName, strconv.Itoa(backupID))
	if err != nil {
		return fmt.Errorf("failed to upload backup: %w", err)
	}
	if !strings.Contains(output, "Backup uploaded successfully") {
		return fmt.Errorf("expected upload to contain 'Backup uploaded successfully', got: %q", output)
	}

	log.Println("Step 7: Verifying backup shows Local+S3")
	output, err = h.runCLI("backup", "list", "--instance", instanceName)
	if err != nil {
		return fmt.Errorf("failed to list backups after upload: %w", err)
	}
	if !strings.Contains(output, "Local+S3") {
		return fmt.Errorf("expected backup to show Local+S3 after upload, got: %q", output)
	}

	log.Println("Step 8: Removing local copy")
	output, err = h.runCLI("backup", "remove-local", instanceName, strconv.Itoa(backupID))
	if err != nil {
		return fmt.Errorf("failed to remove local copy: %w", err)
	}
	if !strings.Contains(output, "Successfully removed local copy") {
		return fmt.Errorf("expected remove-local to contain 'Successfully removed local copy', got: %q", output)
	}

	log.Println("Step 9: Verifying backup shows only S3")
	output, err = h.runCLI("backup", "list", "--instance", instanceName)
	if err != nil {
		return fmt.Errorf("failed to list backups after local removal: %w", err)
	}
	if !strings.Contains(output, "S3") || strings.Contains(output, "Local+S3") {
		return fmt.Errorf("expected backup to show only S3 location after local removal, got: %q", output)
	}

	log.Println("Step 10: Testing download with invalid backup ID")
	output, err = h.runCLI("backup", "download", instanceName, "99999")
	if err == nil {
		return fmt.Errorf("download with invalid backup ID should fail, but succeeded with output: %q", output)
	}
	if !strings.Contains(err.Error(), "backup not found") {
		return fmt.Errorf("expected error for invalid backup ID to contain 'backup not found', got: %v", err)
	}

	log.Println("Step 11: Testing download with wrong instance name")
	output, err = h.runCLI("backup", "download", "wrong-instance", strconv.Itoa(backupID))
	if err == nil {
		return fmt.Errorf("download with wrong instance should fail, but succeeded with output: %q", output)
	}
	// Should fail with either "instance not found" or "backup does not belong"
	if !strings.Contains(err.Error(), "does not belong") && !strings.Contains(err.Error(), "instance not found") {
		return fmt.Errorf("expected error for wrong instance to contain 'does not belong' or 'instance not found', got: %v", err)
	}

	log.Println("Step 12: Downloading backup from S3")
	output, err = h.runCLI("backup", "download", instanceName, strconv.Itoa(backupID))
	if err != nil {
		return fmt.Errorf("failed to download backup: %w (output: %q)", err, output)
	}
	if !strings.Contains(output, "Successfully downloaded backup") {
		return fmt.Errorf("expected download to contain 'Successfully downloaded backup', got: %q", output)
	}
	if !strings.Contains(output, "Local path:") {
		return fmt.Errorf("expected download output to contain 'Local path:', got: %q", output)
	}
	if !strings.Contains(output, "Size:") {
		return fmt.Errorf("expected download output to contain 'Size:', got: %q", output)
	}

	log.Println("Step 13: Verifying backup shows Local+S3 after download")
	output, err = h.runCLI("backup", "list", "--instance", instanceName)
	if err != nil {
		return fmt.Errorf("failed to list backups after download: %w", err)
	}
	if !strings.Contains(output, "Local+S3") {
		return fmt.Errorf("expected backup to show Local+S3 after download, got: %q", output)
	}

	log.Println("Step 14: Testing re-download prevention")
	output, err = h.runCLI("backup", "download", instanceName, strconv.Itoa(backupID))
	if err == nil {
		return fmt.Errorf("re-download should fail when local copy exists, but succeeded with output: %q", output)
	}
	if !strings.Contains(err.Error(), "already has a local copy") {
		return fmt.Errorf("expected re-download error to contain 'already has a local copy', got: %v", err)
	}

	log.Println("Step 15: Creating second backup without upload")
	_, err = h.runCLI("backup", "make", instanceName)
	if err != nil {
		return fmt.Errorf("failed to create second backup: %w", err)
	}

	output, err = h.runCLI("backup", "list", "--instance", instanceName)
	if err != nil {
		return fmt.Errorf("failed to list backups: %w", err)
	}

	lines = strings.Split(output, "\n")
	var newBackupID int
	for i, line := range lines {
		if i == 0 || line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			id, err := strconv.Atoi(fields[0])
			if err == nil && id != backupID {
				newBackupID = id
				break
			}
		}
	}
	if newBackupID == 0 {
		return fmt.Errorf("could not find new backup ID, got: %q", output)
	}

	log.Println("Step 16: Testing download without remote copy")
	output, err = h.runCLI("backup", "download", instanceName, strconv.Itoa(newBackupID))
	if err == nil {
		return fmt.Errorf("download without remote copy should fail, but succeeded with output: %q", output)
	}
	if !strings.Contains(err.Error(), "no remote copy to download") {
		return fmt.Errorf("expected error to contain 'no remote copy to download', got: %v", err)
	}

	log.Println("Step 17: Testing download after removing offsite config")
	// First upload the second backup
	_, err = h.runCLI("backup", "upload", instanceName, strconv.Itoa(newBackupID))
	if err != nil {
		return fmt.Errorf("failed to upload second backup: %w", err)
	}

	_, err = h.runCLI("backup", "remove-local", instanceName, strconv.Itoa(newBackupID))
	if err != nil {
		return fmt.Errorf("failed to remove local copy of second backup: %w", err)
	}

	_, err = h.runCLI("offsite", "remove", "--force")
	if err != nil {
		return fmt.Errorf("failed to remove offsite config: %w", err)
	}

	// Try to download (should fail)
	output, err = h.runCLI("backup", "download", instanceName, strconv.Itoa(newBackupID))
	if err == nil {
		return fmt.Errorf("download without offsite config should fail, but succeeded with output: %q", output)
	}
	if !strings.Contains(err.Error(), "offsite backup not configured") {
		return fmt.Errorf("expected error to contain 'offsite backup not configured', got: %v", err)
	}

	log.Println("Step 18: Cleaning up")
	output, err = h.runCLI("instance", "destroy", instanceName, "--force")
	if err != nil {
		return fmt.Errorf("failed to destroy instance: %w (output: %q)", err, output)
	}

	log.Println("=== Backup Download from S3 Test PASSED ===")
	return nil
}
