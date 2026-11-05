//go:build oddk_debug

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/andrianbdn/oddk/internal/operations"
)

// testSnapshotVerify covers `oddk snapshot verify`: an intact copy passes, and
// every way a stored copy can go bad behind ODDK's back (bit rot, a replaced
// object, a vanished file) is reported and makes the command exit non-zero.
//
// Instance-free like testSnapshotUpload: verify reads archives and the
// catalogue, not instances.
func testSnapshotVerify(h *TestHarness) error {
	log.Println("=== Testing Snapshot Verify ===")

	log.Println("Step 1: A fresh local snapshot verifies")
	if output, err := h.runCLI("snapshot", "make"); err != nil {
		return fmt.Errorf("snapshot make: %w (output: %s)", err, output)
	}
	records, err := listSnapshotRecords(h)
	if err != nil {
		return err
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 snapshot, got %d", len(records))
	}
	rec := records[0]
	id := fmt.Sprintf("%d", rec.ID)

	output, err := h.runCLI("snapshot", "verify", id)
	if err != nil {
		return fmt.Errorf("verify of an intact snapshot failed: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "verified (SHA-256 matches the catalogue)") {
		return fmt.Errorf("verify output does not report the local copy verified: %s", output)
	}

	log.Println("Step 2: Upload, then the S3 copy is checked by HEAD, and by download with --deep")
	if err := applyOffsiteFakeS3(h, "test-snapshot-verify", "oddk-snapshots/"); err != nil {
		return err
	}
	if output, err := h.runCLI("snapshot", "upload", id); err != nil {
		return fmt.Errorf("snapshot upload: %w (output: %s)", err, output)
	}
	records, err = listSnapshotRecords(h)
	if err != nil {
		return err
	}
	rec = records[0]

	result, err := verifyJSON(h, id)
	if err != nil {
		return err
	}
	if !result.OK || result.Local == nil || result.Local.Status != "verified" || result.Remote == nil || result.Remote.Status != "present" {
		return fmt.Errorf("after upload: want local verified + remote present, got %+v", result)
	}
	result, err = verifyJSON(h, id, "--deep")
	if err != nil {
		return err
	}
	if !result.OK || result.Remote == nil || result.Remote.Status != "verified" || result.Remote.SHA256 != rec.SHA256 {
		return fmt.Errorf("--deep: want remote verified with the catalogue digest, got %+v", result.Remote)
	}

	log.Println("Step 3: A replaced S3 object fails the size check; bit rot in it needs --deep")
	original, err := os.ReadFile(rec.LocalLocation)
	if err != nil {
		return err
	}
	if err := replaceFakeS3Object(h, rec.RemoteLocation, "not the archive this host wrote"); err != nil {
		return err
	}
	output, err = h.runCLI("snapshot", "verify", id)
	if err == nil {
		return fmt.Errorf("verify passed a replaced S3 object of a different size: %s", output)
	}
	if !strings.Contains(output, "damaged") || !strings.Contains(output, "bytes") {
		return fmt.Errorf("verify did not report the S3 copy's size as wrong: %s", output)
	}

	// Same size, one flipped bit: HEAD cannot see it, only reading it can.
	// (A same-size substitute archive, caught by the digest, is unit-tested.)
	flipped := append([]byte(nil), original...)
	flipped[len(flipped)/2] ^= 0x01
	if err := putFakeS3Object(h, rec.RemoteLocation, flipped); err != nil {
		return err
	}
	result, err = verifyJSON(h, id)
	if err != nil || result.Remote == nil || result.Remote.Status != "present" {
		return fmt.Errorf("without --deep a same-size object is only known present, got %+v (err %v)", result, err)
	}
	result, err = verifyJSON(h, id, "--deep")
	if err == nil {
		return fmt.Errorf("verify --deep passed a bit-flipped S3 object")
	}
	if result == nil || result.Remote == nil || result.Remote.Status != "damaged" {
		return fmt.Errorf("verify --deep: want remote damaged, got %+v (err %v)", result, err)
	}
	if result.Local == nil || result.Local.Status != "verified" {
		return fmt.Errorf("the intact local copy should still verify, got %+v", result.Local)
	}
	if err := putFakeS3Object(h, rec.RemoteLocation, original); err != nil {
		return err
	}

	log.Println("Step 4: Bit rot in the local copy is reported, and --all exits non-zero")
	// Snapshot names have one-second resolution.
	time.Sleep(1100 * time.Millisecond)
	if output, err := h.runCLI("snapshot", "make"); err != nil {
		return fmt.Errorf("second snapshot make: %w (output: %s)", err, output)
	}
	rotted := append([]byte(nil), original...)
	rotted[len(rotted)/2] ^= 0x01
	if err := os.WriteFile(rec.LocalLocation, rotted, 0o600); err != nil {
		return err
	}
	output, err = h.runCLI("snapshot", "verify", "--all")
	if err == nil {
		return fmt.Errorf("verify --all passed with a damaged local copy: %s", output)
	}
	if !strings.Contains(err.Error(), "1 of 2 snapshot(s) failed verification") {
		return fmt.Errorf("verify --all should fail exactly one of two snapshots, got: %v (output: %s)", err, output)
	}
	if !strings.Contains(output, "damaged") {
		return fmt.Errorf("verify --all did not report the damaged copy: %s", output)
	}

	// --json keeps stdout one parseable document and still exits non-zero.
	output, err = h.runCLI("snapshot", "verify", "--all", "--json")
	if err == nil {
		return fmt.Errorf("verify --all --json passed with a damaged local copy")
	}
	var all []operations.VerifySnapshotResult
	if jsonErr := json.Unmarshal([]byte(output), &all); jsonErr != nil {
		return fmt.Errorf("verify --all --json is not one JSON array: %w (output: %s)", jsonErr, output)
	}
	if len(all) != 2 {
		return fmt.Errorf("verify --all --json returned %d results, want 2", len(all))
	}
	for _, r := range all {
		damaged := r.ID == rec.ID
		if r.OK == damaged {
			return fmt.Errorf("snapshot %d: ok=%t, want %t", r.ID, r.OK, !damaged)
		}
	}

	log.Println("Step 5: A local copy deleted behind ODDK's back is reported missing")
	if err := os.Remove(rec.LocalLocation); err != nil {
		return err
	}
	result, err = verifyJSON(h, id)
	if err == nil {
		return fmt.Errorf("verify passed with the local copy gone")
	}
	if result == nil || result.Local == nil || result.Local.Status != "missing" {
		return fmt.Errorf("want local missing, got %+v (err %v)", result, err)
	}
	// The S3 copy is still fine and says so.
	if result.Remote == nil || result.Remote.Status != "present" {
		return fmt.Errorf("the intact S3 copy should still read present, got %+v", result.Remote)
	}

	log.Println("Step 6: Usage errors")
	if _, err := h.runCLI("snapshot", "verify"); err == nil {
		return fmt.Errorf("verify with no id and no --all should fail")
	}
	if _, err := h.runCLI("snapshot", "verify", "99999"); err == nil || !strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("verify of an unknown id should report not found, got %v", err)
	}

	log.Println("=== Snapshot Verify Test PASSED ===")
	return nil
}

// verifyJSON runs `snapshot verify <id> --json`. The result is parsed even when
// the command fails, since a failed verify still prints its report.
func verifyJSON(h *TestHarness, id string, extra ...string) (*operations.VerifySnapshotResult, error) {
	args := append([]string{"snapshot", "verify", id, "--json"}, extra...)
	output, runErr := h.runCLI(args...)
	var result operations.VerifySnapshotResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("snapshot verify: %w (output: %s)", runErr, output)
		}
		return nil, fmt.Errorf("parse verify output %q: %w", output, err)
	}
	return &result, runErr
}
