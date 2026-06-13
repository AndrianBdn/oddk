package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// testCustomImageSwitch tests switching an instance from one image to another
// and validates image-related CLI flags for pull and create.
func testCustomImageSwitch(h *TestHarness) error {
	log.Println("=== Testing Custom Image Switch ===")

	instanceName := testPrefix + "-img-switch"
	port := 15460

	log.Println("Step 1: Pulling postgres:16 via --version")
	output, err := h.pullImageCLI("16")
	if err != nil {
		return fmt.Errorf("pull postgres:16 failed: %w", err)
	}
	if !strings.Contains(output, "postgres:16") {
		return fmt.Errorf("pull output should mention postgres:16, got: %s", output)
	}

	log.Println("Step 2: Pulling postgres:16 via --image flag")
	output, err = h.pullImageWithImageFlagCLI("postgres:16")
	if err != nil {
		return fmt.Errorf("pull via --image failed: %w", err)
	}
	if !strings.Contains(output, "postgres:16") {
		return fmt.Errorf("pull --image output should mention postgres:16, got: %s", output)
	}

	log.Println("Step 3: Verifying version mismatch detection")
	_, err = h.runCLI("pull", "--image", "postgres:16", "--version", "17")
	if err == nil {
		return fmt.Errorf("pull with version mismatch should have failed")
	}
	if !strings.Contains(err.Error(), "suggests PostgreSQL 16") && !strings.Contains(err.Error(), "mismatch") && !strings.Contains(err.Error(), "suggests PostgreSQL") {
		return fmt.Errorf("mismatch error should mention version conflict, got: %v", err)
	}

	log.Println("Step 4: Verifying --version or --image is required")
	_, err = h.runCLI("pull")
	if err == nil {
		return fmt.Errorf("pull with no flags should have failed")
	}

	log.Println("Step 5: Creating instance with postgres:16")
	output, err = h.createInstanceCLI(instanceName, port)
	if err != nil {
		return fmt.Errorf("create instance failed: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, instanceName) {
		return fmt.Errorf("create output should contain instance name: %s", output)
	}

	log.Println("Step 6: Waiting for PostgreSQL to be ready")
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	log.Println("Step 7: Verifying initial image via API")
	statusCode, body, err := h.request("GET", "/api/rdbms/"+instanceName, nil)
	if err != nil {
		return fmt.Errorf("get instance API call failed: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("expected 200, got %d: %s", statusCode, body)
	}
	var inst struct {
		Image   string `json:"image"`
		Version string `json:"version"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &inst); err != nil {
		return fmt.Errorf("unmarshal instance: %w", err)
	}
	if inst.Image != "postgres:16" {
		return fmt.Errorf("expected image postgres:16, got %q", inst.Image)
	}

	log.Println("Step 8: Creating test database before switch")
	_, err = h.createDatabaseCLI(instanceName, "switchtest")
	if err != nil {
		return fmt.Errorf("create database before switch failed: %w", err)
	}

	log.Println("Step 9: Pulling postgres:16-alpine")
	_, err = h.pullImageWithImageFlagCLI("postgres:16-alpine")
	if err != nil {
		return fmt.Errorf("pull postgres:16-alpine failed: %w", err)
	}

	log.Println("Step 10: Verifying switch to a non-pulled (same-major) image fails")
	_, err = h.switchInstanceCLI(instanceName, "postgres:16-bookworm")
	if err == nil {
		return fmt.Errorf("switch to non-pulled image should have failed")
	}
	if !strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("error should mention image not found, got: %v", err)
	}

	log.Println("Step 10b: Verifying cross-major switch is rejected (must use major-upgrade)")
	_, err = h.switchInstanceCLI(instanceName, "postgres:15")
	if err == nil {
		return fmt.Errorf("cross-major switch (16 -> 15) should have been rejected")
	}
	if !strings.Contains(err.Error(), "major-upgrade") {
		return fmt.Errorf("cross-major switch error should point to major-upgrade, got: %v", err)
	}

	// A cross-major switch is invalid user input, so the API must return 400, not 500.
	crossMajorStatus, _, err := h.request("PUT", "/api/rdbms/"+instanceName+"/image", map[string]string{"image": "postgres:15"})
	if err != nil {
		return fmt.Errorf("cross-major switch API call failed: %w", err)
	}
	if crossMajorStatus != http.StatusBadRequest {
		return fmt.Errorf("cross-major switch should return HTTP 400, got %d", crossMajorStatus)
	}

	log.Println("Step 11: Switching instance to postgres:16-alpine")
	output, err = h.switchInstanceCLI(instanceName, "postgres:16-alpine")
	if err != nil {
		return fmt.Errorf("switch to postgres:16-alpine failed: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "switched successfully") {
		return fmt.Errorf("switch output should indicate success: %s", output)
	}
	if !strings.Contains(output, "postgres:16-alpine") {
		return fmt.Errorf("switch output should mention new image: %s", output)
	}

	log.Println("Step 12: Waiting for PostgreSQL to be ready after switch")
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready after switch: %w", err)
	}

	log.Println("Step 13: Verifying new image via API")
	_, body, err = h.request("GET", "/api/rdbms/"+instanceName, nil)
	if err != nil {
		return fmt.Errorf("get instance API call failed after switch: %w", err)
	}
	if err := json.Unmarshal(body, &inst); err != nil {
		return fmt.Errorf("unmarshal instance after switch: %w", err)
	}
	if inst.Image != "postgres:16-alpine" {
		return fmt.Errorf("expected image postgres:16-alpine after switch, got %q", inst.Image)
	}
	if inst.Version != "16" {
		return fmt.Errorf("expected version 16 after switch, got %q", inst.Version)
	}
	if inst.Status != "running" {
		return fmt.Errorf("expected status running after switch, got %q", inst.Status)
	}

	log.Println("Step 14: Verifying data persists after switch")
	dbListOutput, err := h.listDatabasesCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list databases after switch failed: %w", err)
	}
	if !strings.Contains(dbListOutput, "switchtest") {
		return fmt.Errorf("test database 'switchtest' should persist after switch: %s", dbListOutput)
	}

	// Backup must succeed against the switched (custom) image — regression for the hardcoded
	// postgres:<version> helper image bug that broke backups for custom-image instances.
	log.Println("Step 15: Backing up instance after image switch")
	if _, err := h.backupInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("backup after image switch failed: %w", err)
	}
	backupListOutput, err := h.listBackupsCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list backups after switch failed: %w", err)
	}
	if !strings.Contains(backupListOutput, instanceName) {
		return fmt.Errorf("backup list should include the instance: %s", backupListOutput)
	}

	log.Println("Step 16: Verifying switch to same image fails")
	_, err = h.switchInstanceCLI(instanceName, "postgres:16-alpine")
	if err == nil {
		return fmt.Errorf("switching to same image should have failed")
	}
	if !strings.Contains(err.Error(), "already uses image") {
		return fmt.Errorf("error should mention already uses image, got: %v", err)
	}

	log.Println("Step 17: Cleaning up")
	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	log.Println("=== Custom Image Switch Test PASSED ===")
	return nil
}
