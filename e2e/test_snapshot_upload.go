package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// testSnapshotUpload covers shipping a snapshot offsite: the manual upload
// command, the catalogue's dual-location bookkeeping, and the refusals.
//
// Instance-free like testSnapshotCron — the upload path cares about the archive
// and the catalogue, not about what is inside the archive.
func testSnapshotUpload(h *TestHarness) error {
	log.Println("=== Testing Snapshot Upload ===")

	log.Println("Step 1: A snapshot to upload")
	output, err := h.runCLI("snapshot", "make")
	if err != nil {
		return fmt.Errorf("snapshot make: %w (output: %s)", err, output)
	}

	records, err := listSnapshotRecords(h)
	if err != nil {
		return err
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 catalogue record after 'snapshot make', got %d", len(records))
	}
	id := records[0].ID
	if records[0].LocalLocation == "" {
		return fmt.Errorf("fresh snapshot has no local location")
	}
	if records[0].RemoteLocation != "" {
		return fmt.Errorf("fresh snapshot already claims a remote copy")
	}

	log.Println("Step 2: Upload is refused before offsite is configured")
	output, err = h.runCLI("snapshot", "upload", fmt.Sprintf("%d", id))
	if err == nil {
		return fmt.Errorf("upload should fail without offsite configuration; output: %s", output)
	}
	if !strings.Contains(err.Error(), "offsite backup not configured") {
		return fmt.Errorf("expected 'offsite backup not configured', got: %v", err)
	}

	log.Println("Step 3: Configuring offsite")
	config := map[string]any{
		"type":            "s3",
		"bucket":          "test-snapshot-uploads",
		"endpoint":        h.fakeS3URL,
		"accessKeyId":     "test-key",
		"secretAccessKey": "test-secret",
		"bucketPath":      "oddk-snapshots/",
		"region":          "us-east-1",
	}
	configFile := filepath.Join(h.dataDir, "offsite-config.json")
	configData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(configFile, configData, 0o600); err != nil {
		return err
	}
	if output, err = h.runCLI("offsite", "apply", "--file", configFile); err != nil {
		return fmt.Errorf("apply offsite config: %w (output: %s)", err, output)
	}

	log.Println("Step 4: Uploading the snapshot")
	output, err = h.runCLI("snapshot", "upload", fmt.Sprintf("%d", id))
	if err != nil {
		return fmt.Errorf("snapshot upload: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "s3://test-snapshot-uploads/") {
		return fmt.Errorf("upload output does not report the s3 location: %s", output)
	}
	// Snapshots belong to no instance, so their key is namespaced under a
	// sentinel that cannot collide with one.
	if !strings.Contains(output, "*snapshots*") {
		return fmt.Errorf("upload did not use the snapshot key namespace: %s", output)
	}

	log.Println("Step 5: The catalogue records BOTH copies")
	records, err = listSnapshotRecords(h)
	if err != nil {
		return err
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 record, got %d", len(records))
	}
	if records[0].LocalLocation == "" || records[0].RemoteLocation == "" {
		return fmt.Errorf("after upload the record should carry both copies, got local=%q remote=%q",
			records[0].LocalLocation, records[0].RemoteLocation)
	}

	// The human listing must make "where does this actually exist" obvious.
	output, err = h.runCLI("snapshot", "list")
	if err != nil {
		return fmt.Errorf("snapshot list: %w", err)
	}
	if !strings.Contains(output, "local+s3") {
		return fmt.Errorf("snapshot list does not show both copies: %s", output)
	}

	log.Println("Step 6: Re-uploading the same snapshot is refused")
	output, err = h.runCLI("snapshot", "upload", fmt.Sprintf("%d", id))
	if err == nil {
		return fmt.Errorf("re-upload should be refused; output: %s", output)
	}
	if !strings.Contains(err.Error(), "already uploaded") {
		return fmt.Errorf("expected 'already uploaded', got: %v", err)
	}

	log.Println("Step 7: Uploading a snapshot that does not exist is refused")
	if _, err = h.runCLI("snapshot", "upload", "99999"); err == nil {
		return fmt.Errorf("uploading a nonexistent snapshot should fail")
	}

	log.Println("Step 7b: Round trip — drop the local copy, then download it back")
	// This is the case that makes the offsite copy worth having: retention has
	// removed the local archive and the only way back is through S3. If download
	// did not exist, the primary DR artifact would be unreachable.
	if output, err = h.runCLI("snapshot", "remove-local", fmt.Sprintf("%d", id), "--force"); err != nil {
		return fmt.Errorf("remove-local: %w (output: %s)", err, output)
	}
	records, err = listSnapshotRecords(h)
	if err != nil {
		return err
	}
	if len(records) != 1 {
		return fmt.Errorf("removing the local copy dropped the record even though an S3 copy remains")
	}
	if records[0].LocalLocation != "" {
		return fmt.Errorf("local location survived remove-local: %q", records[0].LocalLocation)
	}
	if records[0].RemoteLocation == "" {
		return fmt.Errorf("remove-local also destroyed the remote copy")
	}

	output, err = h.runCLI("snapshot", "download", fmt.Sprintf("%d", id))
	if err != nil {
		return fmt.Errorf("snapshot download: %w (output: %s)", err, output)
	}
	records, err = listSnapshotRecords(h)
	if err != nil {
		return err
	}
	if records[0].LocalLocation == "" {
		return fmt.Errorf("download did not record a local copy")
	}
	if _, statErr := os.Stat(records[0].LocalLocation); statErr != nil {
		return fmt.Errorf("download recorded %q but no file is there: %v", records[0].LocalLocation, statErr)
	}

	log.Println("Step 7c: Removing the last copy removes the record")
	// snapshot_history CHECKs that a row describes at least one copy, so the
	// record must go with the final copy rather than becoming a phantom.
	if output, err = h.runCLI("snapshot", "remove-remote", fmt.Sprintf("%d", id), "--force"); err != nil {
		return fmt.Errorf("remove-remote: %w (output: %s)", err, output)
	}
	if output, err = h.runCLI("snapshot", "remove-local", fmt.Sprintf("%d", id), "--force"); err != nil {
		return fmt.Errorf("remove-local (last copy): %w (output: %s)", err, output)
	}
	records, err = listSnapshotRecords(h)
	if err != nil {
		return err
	}
	if len(records) != 0 {
		return fmt.Errorf("removing the last copy left %d record(s) describing nothing", len(records))
	}

	log.Println("Step 8: The upload is in the offsite log")
	output, err = h.runCLI("offsite", "logs")
	if err != nil {
		return fmt.Errorf("offsite logs: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "snapshot_upload") {
		return fmt.Errorf("offsite logs do not record the snapshot upload: %s", output)
	}

	log.Println("=== Snapshot Upload Test PASSED ===")
	return nil
}

type e2eSnapshotRecord struct {
	ID             int    `json:"id"`
	Filename       string `json:"filename"`
	Status         string `json:"status"`
	LocalLocation  string `json:"localLocation"`
	RemoteLocation string `json:"remoteLocation"`
}

func listSnapshotRecords(h *TestHarness) ([]e2eSnapshotRecord, error) {
	out, err := h.runCLI("snapshot", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("snapshot list --json: %w (output: %s)", err, out)
	}
	var records []e2eSnapshotRecord
	if err := json.Unmarshal([]byte(out), &records); err != nil {
		return nil, fmt.Errorf("parse snapshot list %q: %w", out, err)
	}
	return records, nil
}
