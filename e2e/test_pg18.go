package main

import (
	"fmt"
	"log"
	"strings"
)

func testPG18Lifecycle(h *TestHarness) error {
	log.Println("=== Testing PG18 Lifecycle ===")

	instanceName := testPrefix + "-pg18"
	port := 15450

	log.Println("Step 1: Pulling PostgreSQL 18 image")
	output, err := h.pullImageCLI("18")
	if err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}
	if !strings.Contains(output, "postgres:18") {
		return fmt.Errorf("pull output should mention postgres:18, got: %s", output)
	}

	log.Println("Step 2: Creating PG 18 instance")
	output, err = h.runCLI("create",
		"--name", instanceName,
		"--version", "18",
		"--port", fmt.Sprintf("%d", port),
		"--cpu", "1",
		"--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance failed: %w (output: %s)", err, output)
	}

	log.Println("Step 3: Waiting for PostgreSQL to be ready")
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	log.Println("Step 4: Listing databases")
	output, err = h.listDatabasesCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list databases failed: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "postgres") {
		return fmt.Errorf("list databases should contain 'postgres' database: %s", output)
	}

	log.Println("Step 5: Creating test database")
	output, err = h.createDatabaseCLI(instanceName, "testdb")
	if err != nil {
		return fmt.Errorf("create database failed: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "created successfully") {
		return fmt.Errorf("create database output should indicate success: %s", output)
	}

	log.Println("Step 6: Destroying instance")
	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	log.Println("=== PG18 Lifecycle Test PASSED ===")
	return nil
}
