package main

import (
	"fmt"
	"strings"
)

func testPasswordOperations(h *TestHarness) error {
	instanceName := testPrefix + "-password"
	port := 25432

	// Pull image first
	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("pull image: %w", err)
	}

	_, err := h.createInstanceCLI(instanceName, port)
	if err != nil {
		return fmt.Errorf("create instance: %w", err)
	}

	// Wait for PostgreSQL to be ready
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	// Test 1: Get password with default format
	output, err := h.getPasswordCLI(instanceName, "")
	if err != nil {
		return fmt.Errorf("get password (default format): %w", err)
	}

	// Verify output contains expected fields
	if !strings.Contains(output, "Instance:") {
		return fmt.Errorf("default format should contain 'Instance:' field")
	}
	if !strings.Contains(output, "Password:") {
		return fmt.Errorf("default format should contain 'Password:' field")
	}
	if !strings.Contains(output, "Connection String:") {
		return fmt.Errorf("default format should contain 'Connection String:' field")
	}
	if !strings.Contains(output, instanceName) {
		return fmt.Errorf("output should contain instance name %s", instanceName)
	}

	// Test 2: Get password in plain format
	plainOutput, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("get password (plain format): %w", err)
	}

	// Plain output should just be the password (one line)
	plainPassword := strings.TrimSpace(plainOutput)
	if strings.Contains(plainPassword, "\n") {
		return fmt.Errorf("plain format should only contain password, got: %s", plainOutput)
	}
	if len(plainPassword) < 20 {
		return fmt.Errorf("password seems too short: %s", plainPassword)
	}

	// Test 3: Get password as connection string
	connOutput, err := h.getPasswordCLI(instanceName, "--conn")
	if err != nil {
		return fmt.Errorf("get password (conn format): %w", err)
	}

	connStr := strings.TrimSpace(connOutput)
	if !strings.HasPrefix(connStr, "postgresql://postgres:") {
		return fmt.Errorf("connection string should start with 'postgresql://postgres:', got: %s", connStr)
	}
	if !strings.Contains(connStr, fmt.Sprintf("10.88.0.1:%d", port)) {
		return fmt.Errorf("connection string should contain '10.88.0.1:%d', got: %s", port, connStr)
	}

	// Test 4: Get password as environment variables
	envOutput, err := h.getPasswordCLI(instanceName, "--envs")
	if err != nil {
		return fmt.Errorf("get password (envs format): %w", err)
	}

	expectedEnvs := []string{
		"export PGHOST=10.88.0.1",
		fmt.Sprintf("export PGPORT=%d", port),
		"export PGUSER=postgres",
		"export PGPASSWORD=",
		"export PGDATABASE=postgres",
	}

	for _, expectedEnv := range expectedEnvs {
		if !strings.Contains(envOutput, expectedEnv) {
			return fmt.Errorf("envs format should contain '%s', got: %s", expectedEnv, envOutput)
		}
	}

	// Test 5: Set password (happy path - set same password)
	// First get the current password
	currentPassword := plainPassword

	setOutput, err := h.setPasswordCLI(instanceName, currentPassword)
	if err != nil {
		return fmt.Errorf("set password failed: %w", err)
	}

	if !strings.Contains(setOutput, "Password updated successfully") {
		return fmt.Errorf("set password should report success, got: %s", setOutput)
	}

	// Verify we can still get the password (it should be the same)
	verifyOutput, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("get password after set failed: %w", err)
	}

	verifyPassword := strings.TrimSpace(verifyOutput)
	if verifyPassword != currentPassword {
		return fmt.Errorf("password should remain the same after setting to same value, expected %s, got %s",
			currentPassword, verifyPassword)
	}

	// Test 6: Verify instance is still functional after password operations
	statusOutput, err := h.getInstanceStatusCLI(instanceName)
	if err != nil {
		return fmt.Errorf("get status after password operations: %w", err)
	}

	if !strings.Contains(statusOutput, "running") {
		return fmt.Errorf("instance should still be running after password operations")
	}

	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("destroy instance: %w", err)
	}

	return nil
}
