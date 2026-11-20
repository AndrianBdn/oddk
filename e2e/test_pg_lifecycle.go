//go:build oddk_debug

package main

import (
	"fmt"
	"log"
	"strings"
)

func testPG18Lifecycle(h *TestHarness) error { return pgLifecycle(h, "18", 15450) }
func testPG19Lifecycle(h *TestHarness) error { return pgLifecycle(h, "19", 15455) }

// pgLifecycle is the smoke test every supported major from 18 on gets: pull,
// create (PG 18+ data-dir layout), list and create databases, destroy.
func pgLifecycle(h *TestHarness, version string, port int) error {
	log.Printf("=== Testing PG%s Lifecycle ===", version)

	instanceName := testPrefix + "-pg" + version

	log.Printf("Step 1: Pulling PostgreSQL %s image", version)
	output, err := h.pullImageCLI(version)
	if err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}
	if !strings.Contains(output, "postgres:"+version) {
		return fmt.Errorf("pull output should mention postgres:%s, got: %s", version, output)
	}

	log.Printf("Step 2: Creating PG %s instance", version)
	output, err = h.runCLI("create",
		"--name", instanceName,
		"--version", version,
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

	log.Printf("=== PG%s Lifecycle Test PASSED ===", version)
	return nil
}
