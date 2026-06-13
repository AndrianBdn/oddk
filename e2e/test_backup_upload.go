package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func testBackupUploadToS3(h *TestHarness) error {
	log.Println("=== Testing Backup Upload to S3 ===")

	log.Println("Step 1: Pulling PostgreSQL 17 image")
	_, err := h.pullImageCLI("17")
	if err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	log.Println("Step 1: Creating test instance")
	instanceName := fmt.Sprintf("oddk-danger-funct-backup-s3-%d", time.Now().Unix())

	output, err := h.runCLI("create",
		"--name", instanceName,
		"--version", "17",
		"--port", "15432",
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
	if err := h.waitForPostgreSQL(15432); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	log.Println("Step 3: Creating backup")
	output, err = h.runCLI("backup", "make", instanceName)
	if err != nil {
		return fmt.Errorf("failed to create backup: %w", err)
	}
	if !strings.Contains(output, "Backup completed successfully") {
		return fmt.Errorf("expected backup output to contain 'Backup completed successfully', got: %q", output)
	}

	log.Println("Step 4: Listing backups to get ID")
	output, err = h.runCLI("backup", "list", "--instance", instanceName)
	if err != nil {
		return fmt.Errorf("failed to list backups: %w", err)
	}

	// Parse backup ID from output (first column, first data row)
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
		return fmt.Errorf("could not find backup ID in list-backups output, got: %q", output)
	}
	log.Printf("Found backup ID: %d", backupID)

	log.Println("Step 5: Testing upload without offsite configuration")
	output, err = h.runCLI("backup", "upload", instanceName, strconv.Itoa(backupID))
	if err == nil {
		return fmt.Errorf("expected upload to fail without offsite config, but it succeeded")
	}
	if !strings.Contains(output, "offsite backup not configured") && !strings.Contains(err.Error(), "offsite backup not configured") {
		return fmt.Errorf("expected error to contain 'offsite backup not configured', got output=%q, err=%v", output, err)
	}

	log.Println("Step 6: Configuring offsite backup")
	log.Printf("FakeS3 URL: %s", h.fakeS3URL)
	config := map[string]any{
		"type":            "s3",
		"bucket":          "test-backup-uploads",
		"endpoint":        h.fakeS3URL,
		"accessKeyId":     "test-key",
		"secretAccessKey": "test-secret",
		"bucketPath":      "oddk-backups/",
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

	log.Println("Step 7: Testing offsite configuration")
	output, err = h.runCLI("offsite", "test")
	if err != nil {
		return fmt.Errorf("failed to test offsite config: %w (output: %q)", err, output)
	}
	if !strings.Contains(output, "✓ Test passed") {
		return fmt.Errorf("expected offsite test to contain '✓ Test passed', got: %q", output)
	}

	log.Println("Step 8: Uploading backup to S3")
	output, err = h.runCLI("backup", "upload", instanceName, strconv.Itoa(backupID))
	if err != nil {
		return fmt.Errorf("failed to upload backup: %w (output: %q)", err, output)
	}
	if !strings.Contains(output, "Backup uploaded successfully") {
		return fmt.Errorf("expected upload output to contain 'Backup uploaded successfully', got: %q", output)
	}
	if !strings.Contains(output, "S3 Location: s3://") {
		return fmt.Errorf("expected upload output to contain 'S3 Location: s3://', got: %q", output)
	}

	s3LocationRe := regexp.MustCompile(`S3 Location: (s3://[^\s]+)`)
	matches := s3LocationRe.FindStringSubmatch(output)
	if len(matches) != 2 {
		return fmt.Errorf("could not extract S3 location from upload output using regex, got: %q", output)
	}
	s3Location := matches[1]
	log.Printf("Backup uploaded to: %s", s3Location)

	log.Println("Step 9: Verifying backup location updated")
	output, err = h.runCLI("backup", "list", "--instance", instanceName)
	if err != nil {
		return fmt.Errorf("failed to list backups after upload: %w", err)
	}
	// After upload, backup should show "Local+S3" location
	if !strings.Contains(output, "Local+S3") {
		return fmt.Errorf("expected backup list to show Local+S3 location after upload, got: %q", output)
	}

	log.Println("Step 10: Testing re-upload prevention")
	output, err = h.runCLI("backup", "upload", instanceName, strconv.Itoa(backupID))
	if err == nil {
		// Check if it reports already uploaded
		if !strings.Contains(output, "already uploaded") && !strings.Contains(output, "already exists") {
			return fmt.Errorf("expected re-upload to report 'already uploaded' or 'already exists', got: %q", output)
		}
	} else {
		// Error is expected for already uploaded backup
		if !strings.Contains(err.Error(), "already uploaded") {
			return fmt.Errorf("expected re-upload error to contain 'already uploaded', got: %v", err)
		}
	}

	log.Println("Step 11: Creating second backup")
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
		return fmt.Errorf("could not find new backup ID in list output, got: %q", output)
	}

	log.Println("Step 12: Uploading second backup")
	output, err = h.runCLI("backup", "upload", instanceName, strconv.Itoa(newBackupID))
	if err != nil {
		return fmt.Errorf("failed to upload second backup: %w", err)
	}
	if !strings.Contains(output, "Backup uploaded successfully") {
		return fmt.Errorf("expected second upload to contain 'Backup uploaded successfully', got: %q", output)
	}

	log.Println("Step 13: Checking offsite logs")
	output, err = h.runCLI("offsite", "logs", "--limit", "10")
	if err != nil {
		return fmt.Errorf("failed to get offsite logs: %w", err)
	}
	if !strings.Contains(output, "backup_upload") {
		return fmt.Errorf("expected offsite logs to contain 'backup_upload', got: %q", output)
	}

	log.Println("Step 14: Testing upload with invalid backup ID")
	output, err = h.runCLI("backup", "upload", instanceName, "99999")
	if err == nil {
		return fmt.Errorf("upload with invalid backup ID should fail, but succeeded with output: %q", output)
	}
	if !strings.Contains(err.Error(), "backup not found") {
		return fmt.Errorf("expected error for invalid backup ID to contain 'backup not found', got: %v", err)
	}

	log.Println("Step 15: Testing upload with wrong instance name")
	output, err = h.runCLI("backup", "upload", "wrong-instance", strconv.Itoa(backupID))
	if err == nil {
		return fmt.Errorf("upload with wrong instance should fail, but succeeded with output: %q", output)
	}
	// The error could be either "instance not found" or "backup does not belong"
	if !strings.Contains(err.Error(), "does not belong") && !strings.Contains(err.Error(), "instance not found") {
		return fmt.Errorf("expected error for wrong instance to contain 'does not belong' or 'instance not found', got: %v", err)
	}

	log.Println("Step 16: Cleaning up")
	output, err = h.runCLI("instance", "destroy", instanceName, "--force")
	if err != nil {
		return fmt.Errorf("failed to destroy instance: %w (output: %q)", err, output)
	}

	_, err = h.runCLI("offsite", "remove", "--force")
	if err != nil {
		log.Printf("Warning: failed to remove offsite config: %v", err)
	}

	log.Println("=== Backup Upload to S3 Test PASSED ===")
	return nil
}
