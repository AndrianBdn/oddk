package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func testCronBackupCleanup(h *TestHarness) error {
	instanceName := testPrefix + "-cron-cleanup-test"
	port := 15437

	// The debug time machine feature is enabled via KVMap in main.go
	// The fake S3 server is also setup via RunFakeS3 flag in main.go

	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("pull image for cleanup test: %w", err)
	}

	if _, err := h.createInstanceCLI(instanceName, port); err != nil {
		return fmt.Errorf("create instance for cleanup test: %w", err)
	}

	// Wait for instance to be ready
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("wait for PostgreSQL for cleanup test: %w", err)
	}

	if err := h.configureOffsiteBackup(); err != nil {
		return fmt.Errorf("configure offsite backup: %w", err)
	}

	var backupIDs []int
	for i := range 5 {
		backupReq := map[string]string{
			"comment": fmt.Sprintf("Test backup %d", i+1),
		}
		statusCode, resp, err := h.request("POST", fmt.Sprintf("/api/rdbms/%s/backup", instanceName), backupReq)
		if err != nil {
			return fmt.Errorf("create backup %d: %w", i+1, err)
		}
		if statusCode != http.StatusOK {
			return fmt.Errorf("create backup %d failed with status %d: %s", i+1, statusCode, string(resp))
		}

		var backupResp map[string]any
		if err := json.Unmarshal(resp, &backupResp); err != nil {
			return fmt.Errorf("parse backup response: %w", err)
		}
		backupID := int(backupResp["backupId"].(float64))
		backupIDs = append(backupIDs, backupID)

		// Small delay between backups
		time.Sleep(100 * time.Millisecond)
	}

	// Upload all backups to S3 first
	for _, backupID := range backupIDs {
		log.Printf("Uploading backup %d to S3", backupID)
		if err := h.uploadBackupToS3(instanceName, backupID); err != nil {
			return fmt.Errorf("upload backup %d to S3: %w", backupID, err)
		}
	}

	// Verify all backups are in S3
	s3Objects, err := h.listS3Backups()
	if err != nil {
		return fmt.Errorf("list S3 objects before cleanup: %w", err)
	}
	if len(s3Objects) != 5 {
		return fmt.Errorf("expected 5 backups in S3, got %d", len(s3Objects))
	}

	// Create a 6th backup that is NOT uploaded to S3 and age it past local
	// retention. With offsite configured, the cron run must retry its upload
	// (as if a previous night's upload had failed) and only then let local
	// retention prune it — so it ends up S3-only instead of being lost
	// entirely (regression test: aged-out never-uploaded backups used to be
	// deleted outright).
	backupReq := map[string]string{"comment": "Never uploaded"}
	statusCode, resp, err := h.request("POST", fmt.Sprintf("/api/rdbms/%s/backup", instanceName), backupReq)
	if err != nil {
		return fmt.Errorf("create unuploaded backup: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("create unuploaded backup failed with status %d: %s", statusCode, string(resp))
	}
	var unuploadedResp map[string]any
	if err := json.Unmarshal(resp, &unuploadedResp); err != nil {
		return fmt.Errorf("parse unuploaded backup response: %w", err)
	}
	backupIDs = append(backupIDs, int(unuploadedResp["backupId"].(float64)))

	// Use the debug endpoint to shift some backups to the past
	// Shift first 2 backups to 10 days ago (will be cleaned up with 7-day local retention)
	for i := range 2 {
		if err := h.timeShiftBackup(backupIDs[i], 10); err != nil {
			return fmt.Errorf("time shift backup %d: %w", backupIDs[i], err)
		}
	}

	// Shift the unuploaded 6th backup to 10 days ago too (past local retention;
	// the cron run must upload it before retention prunes its local copy)
	if err := h.timeShiftBackup(backupIDs[5], 10); err != nil {
		return fmt.Errorf("time shift backup %d: %w", backupIDs[5], err)
	}

	// Shift 3rd backup to 5 days ago (should not be cleaned up with 7-day retention)
	if err := h.timeShiftBackup(backupIDs[2], 5); err != nil {
		return fmt.Errorf("time shift backup %d: %w", backupIDs[2], err)
	}

	// Shift 4th backup to 20 days ago (will be cleaned up from both local and S3)
	if err := h.timeShiftBackup(backupIDs[3], 20); err != nil {
		return fmt.Errorf("time shift backup %d: %w", backupIDs[3], err)
	}

	// Leave 5th backup (backupIDs[4]) as current (not shifted)

	// Setup cron with 7-day local retention and 14-day remote retention
	output, err := h.cronCreateBackupCLI(instanceName, 3, 7, 14)
	if err != nil {
		return fmt.Errorf("setup cron: %w", err)
	}
	if !strings.Contains(output, "keep local: 7 days") {
		return fmt.Errorf("unexpected cron setup output: %s", output)
	}
	log.Printf("Cron configured with 7-day local and 14-day remote retention")

	// Force run settings are enabled via KVMap in main.go
	// Wait for cron to run (it should run within 2 seconds with our settings)

	time.Sleep(3 * time.Second)

	// List backups to verify cleanup
	backups, err := h.listBackupsCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list backups after cleanup: %w", err)
	}

	log.Printf("Backups after cleanup:\n%s", backups)

	// Parse backup list into id -> location (last column), and count remaining
	// LOCAL backups. Format is like:
	// "1  instance-name  timestamp  size  completed  comment  Local+S3"
	backupLines := strings.Split(strings.TrimSpace(backups), "\n")
	var remainingBackupIDs []int
	backupLocations := map[int]string{}
	for _, line := range backupLines {
		if strings.Contains(line, instanceName) && strings.Contains(line, "completed") {
			fields := strings.Fields(line)
			if len(fields) > 1 {
				if id, err := strconv.Atoi(fields[0]); err == nil {
					location := fields[len(fields)-1]
					backupLocations[id] = location
					if strings.Contains(location, "Local") {
						remainingBackupIDs = append(remainingBackupIDs, id)
						log.Printf("Found remaining LOCAL backup ID: %d", id)
					}
				}
			}
		}
	}

	// We should have 3 local backups remaining:
	// - #1 (10 days old, in S3) - deleted
	// - #2 (10 days old, in S3) - deleted
	// - #3 (5 days old, in S3) - kept
	// - #4 (20 days old, in S3) - deleted
	// - #5 (current, in S3) - kept
	// - #6 (10 days old, never uploaded) - upload retried by cron, then local
	//   copy pruned by retention in the same run (ends S3-only)
	// - Plus 1 new backup created by cron task
	// Total: 3 (backups #3, #5, and new cron backup)
	expectedRemaining := 3
	if len(remainingBackupIDs) != expectedRemaining {
		return fmt.Errorf("expected %d local backups after cleanup, got %d", expectedRemaining, len(remainingBackupIDs))
	}
	log.Printf("Local cleanup successful: %d backups remain", len(remainingBackupIDs))

	// Verify the correct backups lost their local copy (backups 1, 2, 4, and
	// the retried-then-pruned 6)
	deletedBackups := []int{0, 1, 3, 5} // Indices for backups 1, 2, 4 and 6
	for _, idx := range deletedBackups {
		if slices.Contains(remainingBackupIDs, backupIDs[idx]) {
			return fmt.Errorf("backup %d should have been deleted but still exists", backupIDs[idx])
		}
	}

	// Verify backups 3 and 5 are still there locally
	keptBackups := []int{2, 4} // Indices for backups 3 and 5
	for _, idx := range keptBackups {
		if !slices.Contains(remainingBackupIDs, backupIDs[idx]) {
			return fmt.Errorf("backup %d should still exist but was deleted", backupIDs[idx])
		}
	}

	// Verify S3 cleanup - should have 5 backups (4 original after 1 deleted + 1 new from cron)
	// The 4th backup was shifted to 20 days ago, so should be deleted from S3 (>14 days)
	s3ObjectsAfter, err := h.listS3Backups()
	if err != nil {
		return fmt.Errorf("list S3 objects after cleanup: %w", err)
	}
	log.Printf("S3 objects after cleanup: %d", len(s3ObjectsAfter))
	for _, key := range s3ObjectsAfter {
		log.Printf("  S3 object: %s", key)
	}

	// We expect 6 backups in S3:
	// - Original 5 backups were all uploaded
	// - Backup 4 (backupIDs[3]) was 20 days old, so deleted from S3
	// - Backup 6 (backupIDs[5]) was uploaded by the cron retry pass
	// - 1 new backup was created by cron
	// So: 5 - 1 + 1 + 1 = 6
	expectedS3Count := 6
	if len(s3ObjectsAfter) != expectedS3Count {
		return fmt.Errorf("expected %d backups in S3 after cleanup, got %d", expectedS3Count, len(s3ObjectsAfter))
	}

	// S3 keys don't contain backup IDs, so verify per-backup state via the
	// listing's location column instead.
	// Backup 4 (20 days old) lost both its local and S3 copy, so its record
	// is removed entirely from the listing.
	if location, ok := backupLocations[backupIDs[3]]; ok {
		return fmt.Errorf("backup %d (20 days old) should be fully removed but is still listed with location %q", backupIDs[3], location)
	}

	// Backup 6 was uploaded by the cron retry pass and its local copy was then
	// pruned by retention, leaving it S3-only.
	if location := backupLocations[backupIDs[5]]; location != "S3" {
		return fmt.Errorf("backup %d should be S3-only after upload retry + local cleanup, got location %q", backupIDs[5], location)
	}

	log.Printf("S3 cleanup successful: old backup removed, unuploaded backup retried")

	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("cleanup instance: %w", err)
	}

	return nil
}

// configureOffsiteBackup configures S3 offsite backup for testing
func (h *TestHarness) configureOffsiteBackup() error {
	config := map[string]any{
		"type":            "s3",
		"bucket":          "test-backups",
		"endpoint":        h.fakeS3URL,
		"accessKeyId":     "test-key",
		"secretAccessKey": "test-secret",
		"bucketPath":      "cron-cleanup-test/",
		"region":          "us-east-1",
	}

	configFile := filepath.Join(h.dataDir, "offsite-config.json")
	configData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(configFile, configData, 0o600); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}

	output, err := h.runCLI("offsite", "apply", "--file", configFile)
	if err != nil {
		return fmt.Errorf("apply offsite config: %w", err)
	}
	if !strings.Contains(output, "Offsite configuration applied successfully") {
		return fmt.Errorf("unexpected offsite apply output: %s", output)
	}

	// Test the configuration
	output, err = h.runCLI("offsite", "test")
	if err != nil {
		return fmt.Errorf("test offsite config: %w", err)
	}
	if !strings.Contains(output, "✓ Test passed") {
		return fmt.Errorf("offsite test failed: %s", output)
	}

	return nil
}

// uploadBackupToS3 uploads a backup to S3
func (h *TestHarness) uploadBackupToS3(instanceName string, backupID int) error {
	output, err := h.runCLI("backup", "upload", instanceName, strconv.Itoa(backupID))
	if err != nil {
		return fmt.Errorf("upload backup %d: %w (output: %s)", backupID, err, output)
	}
	if !strings.Contains(output, "Backup uploaded successfully") {
		return fmt.Errorf("unexpected upload output: %s", output)
	}
	return nil
}

// listS3Backups lists all backup objects in the S3 bucket
func (h *TestHarness) listS3Backups() ([]string, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test-key", "test-secret", "")),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc( //nolint:staticcheck
			func(service, region string, options ...any) (aws.Endpoint, error) { //nolint:staticcheck
				return aws.Endpoint{ //nolint:staticcheck
					URL:           h.fakeS3URL,
					SigningRegion: "us-east-1",
				}, nil
			})),
	)
	if err != nil {
		return nil, fmt.Errorf("create S3 config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	// List objects in the bucket
	result, err := client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String("test-backups"),
		Prefix: aws.String("cron-cleanup-test/"),
	})
	if err != nil {
		return nil, fmt.Errorf("list S3 objects: %w", err)
	}

	var keys []string
	for _, obj := range result.Contents {
		if obj.Key != nil {
			keys = append(keys, *obj.Key)
		}
	}

	return keys, nil
}

// Helper function to time-shift a backup using the debug endpoint
func (h *TestHarness) timeShiftBackup(backupID, daysBack int) error {
	req := map[string]int{
		"backupId": backupID,
		"daysBack": daysBack,
	}

	statusCode, resp, err := h.request("POST", "/api/debug/backup/time-shift", req)
	if err != nil {
		return fmt.Errorf("time shift request failed: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("time shift request failed with status %d: %s", statusCode, string(resp))
	}

	var result map[string]any
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}

	if message, ok := result["message"].(string); ok {
		fmt.Printf("Time shift: %s\n", message)
	}

	return nil
}
