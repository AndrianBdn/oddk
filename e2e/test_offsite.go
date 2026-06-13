package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func testOffsiteConfiguration(h *TestHarness) error {
	// This test requires a fake S3 server which is set up by the harness

	log.Println("Step 1: Test with no configuration")
	status, body, err := h.request("GET", "/api/offsite", nil)
	if err != nil {
		return fmt.Errorf("failed to get offsite info: %w", err)
	}
	if status != 200 {
		return fmt.Errorf("expected status 200, got %d: %s", status, body)
	}

	var infoResult struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(body, &infoResult); err != nil {
		return fmt.Errorf("failed to parse offsite info response: %w", err)
	}
	if infoResult.Active {
		return fmt.Errorf("expected no active configuration initially")
	}

	log.Println("Step 2: Create first configuration")
	config1 := map[string]any{
		"type":            "s3",
		"bucket":          "test-bucket-1",
		"endpoint":        h.fakeS3URL,
		"accessKeyId":     "test-key",
		"secretAccessKey": "test-secret",
		"bucketPath":      "oddk-backups/",
	}

	configFile1 := filepath.Join(h.dataDir, "config1.json")
	configData1, err := json.MarshalIndent(config1, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config1: %w", err)
	}
	if err := os.WriteFile(configFile1, configData1, 0o600); err != nil {
		return fmt.Errorf("failed to write config1 file: %w", err)
	}

	// Apply first configuration
	output, err := h.runCLI("offsite", "apply", "--file", configFile1)
	if err != nil {
		return fmt.Errorf("failed to apply first config: %w", err)
	}
	if !strings.Contains(output, "Offsite configuration applied successfully") {
		return fmt.Errorf("unexpected apply output: %q", output)
	}

	log.Println("Step 3: Test first configuration")
	output, err = h.runCLI("offsite", "test")
	if err != nil {
		return fmt.Errorf("failed to test first config: %w (output: %q)", err, output)
	}
	if !strings.Contains(output, "✓ Test passed") {
		return fmt.Errorf("first config test failed: %q", output)
	}

	log.Println("Step 4: Verify configuration info shows active")
	status, body, err = h.request("GET", "/api/offsite", nil)
	if err != nil {
		return fmt.Errorf("failed to get offsite info after apply: %w", err)
	}
	if status != 200 {
		return fmt.Errorf("expected status 200, got %d: %s", status, body)
	}

	if err := json.Unmarshal(body, &infoResult); err != nil {
		return fmt.Errorf("failed to parse offsite info response: %w", err)
	}
	if !infoResult.Active {
		return fmt.Errorf("expected active configuration after apply")
	}

	log.Println("Step 5: Create second configuration with different bucket")
	config2 := map[string]any{
		"type":            "s3",
		"bucket":          "test-bucket-2",
		"endpoint":        h.fakeS3URL,
		"accessKeyId":     "test-key",
		"secretAccessKey": "test-secret",
		"bucketPath":      "backups/",
	}

	configFile2 := filepath.Join(h.dataDir, "config2.json")
	configData2, err := json.MarshalIndent(config2, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config2: %w", err)
	}
	if err := os.WriteFile(configFile2, configData2, 0o600); err != nil {
		return fmt.Errorf("failed to write config2 file: %w", err)
	}

	// Apply second configuration
	output, err = h.runCLI("offsite", "apply", "--file", configFile2)
	if err != nil {
		return fmt.Errorf("failed to apply second config: %w", err)
	}
	if !strings.Contains(output, "Offsite configuration applied successfully") {
		return fmt.Errorf("unexpected apply output for config2: %q", output)
	}

	log.Println("Step 6: Test second configuration")
	output, err = h.runCLI("offsite", "test")
	if err != nil {
		return fmt.Errorf("failed to test second config: %w", err)
	}
	if !strings.Contains(output, "✓ Test passed") {
		return fmt.Errorf("second config test failed: %s", output)
	}

	log.Println("Step 7: Test get configuration command")
	output, err = h.runCLI("offsite", "get")
	if err != nil {
		return fmt.Errorf("failed to get config: %w", err)
	}

	var getResult map[string]any
	if err := json.Unmarshal([]byte(output), &getResult); err != nil {
		return fmt.Errorf("failed to parse get config response: %w", err)
	}

	if getResult["bucket"] != "test-bucket-2" {
		return fmt.Errorf("expected bucket to be test-bucket-2, got %v", getResult["bucket"])
	}
	if getResult["secretAccessKey"] != "%SAME-AS-BEFORE%" {
		return fmt.Errorf("expected secretAccessKey placeholder, got %v", getResult["secretAccessKey"])
	}

	log.Println("Step 8: Test with %SAME-AS-BEFORE% placeholder")
	config3 := map[string]any{
		"type":            "s3",
		"bucket":          "test-bucket-3",
		"endpoint":        h.fakeS3URL,
		"accessKeyId":     "test-key",
		"secretAccessKey": "%SAME-AS-BEFORE%", // Use placeholder
		"bucketPath":      "oddk/",
	}

	configFile3 := filepath.Join(h.dataDir, "config3.json")
	configData3, err := json.MarshalIndent(config3, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config3: %w", err)
	}
	if err := os.WriteFile(configFile3, configData3, 0o600); err != nil {
		return fmt.Errorf("failed to write config3 file: %w", err)
	}

	// Apply third configuration with placeholder
	output, err = h.runCLI("offsite", "apply", "--file", configFile3)
	if err != nil {
		return fmt.Errorf("failed to apply third config: %w", err)
	}
	if !strings.Contains(output, "Offsite configuration applied successfully") {
		return fmt.Errorf("unexpected apply output for config3: %q", output)
	}

	// Test third configuration
	output, err = h.runCLI("offsite", "test")
	if err != nil {
		return fmt.Errorf("failed to test third config: %w", err)
	}
	if !strings.Contains(output, "✓ Test passed") {
		return fmt.Errorf("third config test failed: %s", output)
	}

	log.Println("Step 9: Check logs")
	output, err = h.runCLI("offsite", "logs", "--limit", "10")
	if err != nil {
		return fmt.Errorf("failed to get logs: %w", err)
	}
	if !strings.Contains(output, "✓ Success") {
		return fmt.Errorf("expected success entries in logs: %s", output)
	}
	if !strings.Contains(output, "test") {
		return fmt.Errorf("expected test events in logs: %s", output)
	}

	log.Println("Step 10: Test info command")
	output, err = h.runCLI("offsite", "info")
	if err != nil {
		return fmt.Errorf("failed to get info: %w", err)
	}
	if !strings.Contains(output, "test-bucket-3") {
		return fmt.Errorf("expected bucket test-bucket-3 in info: %s", output)
	}
	if !strings.Contains(output, "s3") {
		return fmt.Errorf("expected type s3 in info: %s", output)
	}

	log.Println("Step 11: Test EC2IAMRole with empty secret")
	config4 := map[string]any{
		"type":            "s3",
		"bucket":          "test-bucket-4",
		"endpoint":        h.fakeS3URL,
		"accessKeyId":     "",
		"secretAccessKey": "",
		"bucketPath":      "ec2/",
		"ec2IamRole":      true,
	}

	configFile4 := filepath.Join(h.dataDir, "config4.json")
	configData4, err := json.MarshalIndent(config4, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config4: %w", err)
	}
	if err := os.WriteFile(configFile4, configData4, 0o600); err != nil {
		return fmt.Errorf("failed to write config4 file: %w", err)
	}

	// Apply EC2IAMRole configuration with empty credentials
	output, err = h.runCLI("offsite", "apply", "--file", configFile4)
	if err != nil {
		return fmt.Errorf("failed to apply EC2IAMRole config: %w", err)
	}
	if !strings.Contains(output, "Offsite configuration applied successfully") {
		return fmt.Errorf("unexpected apply output for EC2IAMRole config: %q", output)
	}

	// Test get configuration to verify empty secrets are handled correctly
	output, err = h.runCLI("offsite", "get")
	if err != nil {
		return fmt.Errorf("failed to get EC2IAMRole config: %w", err)
	}

	var getResult4 map[string]any
	if err := json.Unmarshal([]byte(output), &getResult4); err != nil {
		return fmt.Errorf("failed to parse get EC2IAMRole config response: %w", err)
	}

	if getResult4["bucket"] != "test-bucket-4" {
		return fmt.Errorf("expected bucket to be test-bucket-4, got %v", getResult4["bucket"])
	}
	if getResult4["ec2IamRole"] != true {
		return fmt.Errorf("expected ec2IamRole to be true, got %v", getResult4["ec2IamRole"])
	}
	// For empty secrets, we should get empty string back, not the placeholder
	if getResult4["secretAccessKey"] != "" {
		return fmt.Errorf("expected secretAccessKey to be empty, got %v", getResult4["secretAccessKey"])
	}

	// Note: We skip the offsite test for EC2IAMRole config since it requires actual AWS environment

	log.Println("Step 12: Test validation errors")
	// Test invalid bucket name (too short)
	invalidConfig := map[string]any{
		"type":            "s3",
		"bucket":          "ab", // Too short
		"endpoint":        h.fakeS3URL,
		"accessKeyId":     "test-key",
		"secretAccessKey": "test-secret",
	}

	configFileInvalid := filepath.Join(h.dataDir, "config_invalid.json")
	configDataInvalid, err := json.MarshalIndent(invalidConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal invalid config: %w", err)
	}
	if err := os.WriteFile(configFileInvalid, configDataInvalid, 0o600); err != nil {
		return fmt.Errorf("failed to write invalid config file: %w", err)
	}

	// This should fail with validation error
	_, err = h.runCLI("offsite", "apply", "--file", configFileInvalid)
	if err == nil {
		return fmt.Errorf("expected validation error for invalid config, but got none")
	}
	if !strings.Contains(err.Error(), "bucket name must be at least 3 characters") {
		return fmt.Errorf("expected bucket validation error, got: %s", err.Error())
	}

	// Test invalid bucket path (starts with slash)
	invalidPathConfig := map[string]any{
		"type":            "s3",
		"bucket":          "test-bucket",
		"endpoint":        h.fakeS3URL,
		"accessKeyId":     "test-key",
		"secretAccessKey": "test-secret",
		"bucketPath":      "/invalid/path/",
	}

	configFileInvalidPath := filepath.Join(h.dataDir, "config_invalid_path.json")
	configDataInvalidPath, err := json.MarshalIndent(invalidPathConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal invalid path config: %w", err)
	}
	if err := os.WriteFile(configFileInvalidPath, configDataInvalidPath, 0o600); err != nil {
		return fmt.Errorf("failed to write invalid path config file: %w", err)
	}

	// This should fail with validation error
	_, err = h.runCLI("offsite", "apply", "--file", configFileInvalidPath)
	if err == nil {
		return fmt.Errorf("expected validation error for invalid path config, but got none")
	}
	if !strings.Contains(err.Error(), "bucket path must not start with '/'") {
		return fmt.Errorf("expected path validation error, got: %s", err.Error())
	}

	log.Println("Step 13: Remove configuration")
	output, err = h.runCLI("offsite", "remove", "--force")
	if err != nil {
		return fmt.Errorf("failed to remove config: %w", err)
	}
	if !strings.Contains(output, "Offsite configuration removed successfully") {
		return fmt.Errorf("unexpected remove output: %q", output)
	}

	return nil
}
