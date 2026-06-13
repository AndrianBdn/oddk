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

func testParameterGroupInstanceIntegration(h *TestHarness) error {
	// Test 1: Pull image first
	_, err := h.pullImageCLI("16")
	if err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	// Test 2: Create a custom parameter group
	testParamFile := filepath.Join(h.dataDir, "pg-test-params.json")
	testParams := []map[string]any{
		{
			"name":      "shared_buffers",
			"type":      "postgres_cli_arg",
			"valueType": "numeric_mem",
			"value":     "{expr}DBContainerMemoryMB / 8{/expr} MB",
		},
		{
			"name":      "max_connections",
			"type":      "postgres_cli_arg",
			"valueType": "numeric",
			"value":     "50",
		},
		{
			"name":      "work_mem",
			"type":      "postgres_cli_arg",
			"valueType": "numeric_mem",
			"value":     "64 MB",
		},
		{
			"name":      "log_statement",
			"type":      "postgres_cli_arg",
			"valueType": "string",
			"value":     "all",
		},
	}

	testParamsJSON, err := json.MarshalIndent(testParams, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal test parameters: %w", err)
	}

	if err := os.WriteFile(testParamFile, testParamsJSON, 0o600); err != nil {
		return fmt.Errorf("write test parameter file: %w", err)
	}

	// Test 3: Create the parameter group
	createOutput, err := h.createParameterGroupCLI("pg-test-group", testParamFile)
	if err != nil {
		return fmt.Errorf("create parameter group failed: %w", err)
	}

	if !strings.Contains(createOutput, "created successfully") {
		return fmt.Errorf("create parameter group output should indicate success: %s", createOutput)
	}

	// Test 4: Create instance with custom parameter group
	instanceName := "pg-test-instance"
	instancePort := 15433

	createInstanceOutput, err := h.createInstanceWithParameterGroupCLI(instanceName, instancePort, "pg-test-group")
	if err != nil {
		return fmt.Errorf("create instance with parameter group failed: %w", err)
	}

	if !strings.Contains(createInstanceOutput, "Created RDBMS instance") {
		return fmt.Errorf("create instance output should indicate success: %s", createInstanceOutput)
	}

	// Test 5: Start the instance
	startOutput, err := h.startInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("start instance failed: %w", err)
	}

	if !strings.Contains(startOutput, "Started instance") {
		return fmt.Errorf("start instance output should indicate started: %s", startOutput)
	}

	// Test 6: Wait for PostgreSQL to be ready
	if err := h.waitForPostgreSQL(instancePort); err != nil {
		return fmt.Errorf("PostgreSQL did not start in time: %w", err)
	}

	// Test 7: Get instance password for connection
	passwordOutput, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("get password failed: %w", err)
	}

	password := strings.TrimSpace(passwordOutput)
	if password == "" {
		return fmt.Errorf("password should not be empty")
	}

	// Test 8: Connect to PostgreSQL and verify settings
	connStr := fmt.Sprintf("postgresql://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable", password, instancePort)
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("open database connection: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Test connection
	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	// Test 9: Verify parameter settings in PostgreSQL
	expectedSettings := map[string]string{
		"shared_buffers":  "256MB", // 2048MB / 8 = 256MB
		"max_connections": "50",
		"work_mem":        "64MB",
		"log_statement":   "all",
	}

	for setting, expectedValue := range expectedSettings {
		var actualValue string
		err := db.QueryRow("SHOW " + setting).Scan(&actualValue)
		if err != nil {
			return fmt.Errorf("query setting %s: %w", setting, err)
		}

		if actualValue != expectedValue {
			return fmt.Errorf("setting %s: expected %s, got %s", setting, expectedValue, actualValue)
		}
	}

	// Test 10: Verify instance list shows parameter group
	listOutput, err := h.listInstancesCLI()
	if err != nil {
		return fmt.Errorf("list instances failed: %w", err)
	}

	if !strings.Contains(listOutput, instanceName) {
		return fmt.Errorf("list output should contain instance name: %s", listOutput)
	}

	if !strings.Contains(listOutput, "pg-test-group") {
		return fmt.Errorf("list output should show parameter group: %s", listOutput)
	}

	if !strings.Contains(listOutput, "PARAMETER GROUP") {
		return fmt.Errorf("list output should have parameter group header: %s", listOutput)
	}

	// Test 11: Clean up - stop and destroy instance
	_, err = h.stopInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("stop instance failed: %w", err)
	}

	err = h.destroyInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	// Test 12: Clean up parameter group
	_, err = h.deleteParameterGroupCLI("pg-test-group", true)
	if err != nil {
		return fmt.Errorf("delete parameter group failed: %w", err)
	}

	return nil
}

func testDefaultParameterGroupUsage(h *TestHarness) error {
	// Test 1: Pull image first
	_, err := h.pullImageCLI("16")
	if err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	// Test 2: Create instance without specifying parameter group (should use default)
	instanceName := "default-pg-test"
	instancePort := 15434

	createOutput, err := h.createInstanceCLI(instanceName, instancePort)
	if err != nil {
		return fmt.Errorf("create instance failed: %w", err)
	}

	if !strings.Contains(createOutput, "Created RDBMS instance") {
		return fmt.Errorf("create instance output should indicate success: %s", createOutput)
	}

	// Test 3: Verify instance list shows default parameter group
	listOutput, err := h.listInstancesCLI()
	if err != nil {
		return fmt.Errorf("list instances failed: %w", err)
	}

	if !strings.Contains(listOutput, instanceName) {
		return fmt.Errorf("list output should contain instance name: %s", listOutput)
	}

	if !strings.Contains(listOutput, "default:2025-08-27") {
		return fmt.Errorf("list output should show default parameter group: %s", listOutput)
	}

	// Test 4: Start instance and verify some default parameters
	startOutput, err := h.startInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("start instance failed: %w", err)
	}

	if !strings.Contains(startOutput, "Started instance") {
		return fmt.Errorf("start instance output should indicate started: %s", startOutput)
	}

	// Wait for PostgreSQL to be ready
	if err := h.waitForPostgreSQL(instancePort); err != nil {
		return fmt.Errorf("PostgreSQL did not start in time: %w", err)
	}

	passwordOutput, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("get password failed: %w", err)
	}

	password := strings.TrimSpace(passwordOutput)
	connStr := fmt.Sprintf("postgresql://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable", password, instancePort)
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("open database connection: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	// Verify at least one setting from the default parameter group
	var sharedBuffers string
	err = db.QueryRow("SHOW shared_buffers").Scan(&sharedBuffers)
	if err != nil {
		return fmt.Errorf("query shared_buffers: %w", err)
	}

	// shared_buffers should be set by the default parameter group (DBContainerMemoryMB/4 = 1024/4 = 256MB)
	if sharedBuffers != "256MB" {
		return fmt.Errorf("expected shared_buffers to be 256MB from default parameter group, got %s", sharedBuffers)
	}

	// Test 5: Clean up
	_, err = h.stopInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("stop instance failed: %w", err)
	}

	err = h.destroyInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	return nil
}
