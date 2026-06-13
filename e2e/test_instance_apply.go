package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/lib/pq"
)

// testInstanceApply tests applying a new parameter group to an existing instance
func testInstanceApply(h *TestHarness) error {
	// Test 1: Pull image first
	_, err := h.pullImageCLI("16")
	if err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	// Test 2: Create instance with default parameter group
	instanceName := "apply-test-instance"
	instancePort := 15450

	createOutput, err := h.createInstanceCLI(instanceName, instancePort)
	if err != nil {
		return fmt.Errorf("create instance failed: %w", err)
	}

	if !strings.Contains(createOutput, "Created RDBMS instance") {
		return fmt.Errorf("create instance output should indicate success: %s", createOutput)
	}

	// Test 3: Wait for PostgreSQL to be ready
	if err := h.waitForPostgreSQL(instancePort); err != nil {
		return fmt.Errorf("PostgreSQL did not start in time: %w", err)
	}

	// Test 4: Get password for later verification
	passwordOutput, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("get password failed: %w", err)
	}
	password := strings.TrimSpace(passwordOutput)

	// Test 5: Verify initial max_connections (from default group)
	connStr := fmt.Sprintf("postgresql://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable", password, instancePort)
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("open database connection: %w", err)
	}

	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	var initialMaxConn string
	err = db.QueryRow("SHOW max_connections").Scan(&initialMaxConn)
	if err != nil {
		return fmt.Errorf("query initial max_connections: %w", err)
	}
	_ = db.Close()

	// Test 6: Create a custom parameter group with different max_connections
	testParamFile := filepath.Join(h.dataDir, "apply-test-params.json")
	testParams := []map[string]any{
		{
			"name":      "max_connections",
			"type":      "postgres_cli_arg",
			"valueType": "numeric",
			"value":     "75",
		},
		{
			"name":      "shared_buffers",
			"type":      "postgres_cli_arg",
			"valueType": "numeric_mem",
			"value":     "128 MB",
		},
	}

	testParamsJSON, err := json.MarshalIndent(testParams, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal test parameters: %w", err)
	}

	if err := os.WriteFile(testParamFile, testParamsJSON, 0o600); err != nil {
		return fmt.Errorf("write test parameter file: %w", err)
	}

	createGroupOutput, err := h.createParameterGroupCLI("apply-test-group", testParamFile)
	if err != nil {
		return fmt.Errorf("create parameter group failed: %w", err)
	}

	if !strings.Contains(createGroupOutput, "created successfully") {
		return fmt.Errorf("create parameter group output should indicate success: %s", createGroupOutput)
	}

	// Test 7: Apply the new parameter group to the instance
	applyOutput, err := h.applyParameterGroupCLI(instanceName, "apply-test-group")
	if err != nil {
		return fmt.Errorf("apply parameter group failed: %w", err)
	}

	if !strings.Contains(applyOutput, "reconfigured successfully") {
		return fmt.Errorf("apply output should indicate success: %s", applyOutput)
	}

	if !strings.Contains(applyOutput, "apply-test-group") {
		return fmt.Errorf("apply output should show new parameter group: %s", applyOutput)
	}

	// Test 8: Wait for PostgreSQL to be ready again after reconfiguration
	if err := h.waitForPostgreSQL(instancePort); err != nil {
		return fmt.Errorf("PostgreSQL did not restart in time after apply: %w", err)
	}

	// Test 9: Verify max_connections changed to 75
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("reopen database connection: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping database after apply: %w", err)
	}

	var newMaxConn string
	err = db.QueryRow("SHOW max_connections").Scan(&newMaxConn)
	if err != nil {
		return fmt.Errorf("query new max_connections: %w", err)
	}

	if newMaxConn != "75" {
		return fmt.Errorf("expected max_connections to be 75 after apply, got %s (was %s before)", newMaxConn, initialMaxConn)
	}

	// Test 10: Verify shared_buffers also changed
	var newSharedBuffers string
	err = db.QueryRow("SHOW shared_buffers").Scan(&newSharedBuffers)
	if err != nil {
		return fmt.Errorf("query new shared_buffers: %w", err)
	}

	if newSharedBuffers != "128MB" {
		return fmt.Errorf("expected shared_buffers to be 128MB after apply, got %s", newSharedBuffers)
	}

	// Test 11: Verify instance list shows new parameter group
	listOutput, err := h.listInstancesCLI()
	if err != nil {
		return fmt.Errorf("list instances failed: %w", err)
	}

	if !strings.Contains(listOutput, "apply-test-group") {
		return fmt.Errorf("instance list should show new parameter group: %s", listOutput)
	}

	// Test 12: Try to apply the same parameter group (should fail)
	_, err = h.applyParameterGroupCLI(instanceName, "apply-test-group")
	if err == nil {
		return fmt.Errorf("applying same parameter group should fail")
	}

	if !strings.Contains(err.Error(), "already uses parameter group") {
		return fmt.Errorf("error should mention 'already uses parameter group': %v", err)
	}

	// Test 13: Try to apply non-existent parameter group (should fail)
	_, err = h.applyParameterGroupCLI(instanceName, "non-existent-group")
	if err == nil {
		return fmt.Errorf("applying non-existent parameter group should fail")
	}

	// Test 14: Clean up
	_, err = h.stopInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("stop instance failed: %w", err)
	}

	err = h.destroyInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	_, err = h.deleteParameterGroupCLI("apply-test-group", true)
	if err != nil {
		return fmt.Errorf("delete parameter group failed: %w", err)
	}

	return nil
}
