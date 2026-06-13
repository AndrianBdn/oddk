package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func testParameterGroupOperations(h *TestHarness) error {
	// Test 1: List parameter groups (should show default group)
	listOutput, err := h.listParameterGroupsCLI()
	if err != nil {
		return fmt.Errorf("list parameter groups failed: %w", err)
	}

	// Should contain the default parameter group
	if !strings.Contains(listOutput, "default:2025-08-27") {
		return fmt.Errorf("list output should contain default parameter group: %s", listOutput)
	}

	// Should show summary format with counts
	if !strings.Contains(listOutput, "GROUP NAME") {
		return fmt.Errorf("list output should have summary table headers: %s", listOutput)
	}

	if !strings.Contains(listOutput, "postgres_cli_arg:") {
		return fmt.Errorf("list output should show parameter type counts: %s", listOutput)
	}

	// Test 2: Get specific default parameter group
	getOutput, err := h.getParameterGroupCLI("default:2025-08-27")
	if err != nil {
		return fmt.Errorf("get default parameter group failed: %w", err)
	}

	// Should show detailed parameter table
	if !strings.Contains(getOutput, "Parameter Group: default:2025-08-27") {
		return fmt.Errorf("get output should show parameter group name: %s", getOutput)
	}

	// Should contain expected parameters
	expectedParams := []string{"shared_buffers", "max_connections", "work_mem"}
	for _, param := range expectedParams {
		if !strings.Contains(getOutput, param) {
			return fmt.Errorf("get output should contain parameter %s: %s", param, getOutput)
		}
	}

	// Test 3: Get parameter groups as JSON
	jsonOutput, err := h.getParameterGroupsJSONCLI()
	if err != nil {
		return fmt.Errorf("get parameter groups JSON failed: %w", err)
	}

	var jsonResult struct {
		Groups []struct {
			Name       string `json:"name"`
			Parameters []struct {
				Name      string `json:"name"`
				Type      string `json:"type"`
				ValueType string `json:"valueType"`
				Value     string `json:"value"`
			} `json:"parameters"`
		} `json:"groups"`
	}

	if err := json.Unmarshal([]byte(jsonOutput), &jsonResult); err != nil {
		return fmt.Errorf("failed to parse JSON output: %w", err)
	}

	if len(jsonResult.Groups) == 0 {
		return fmt.Errorf("JSON output should contain at least one parameter group")
	}

	// Find default group
	var defaultGroup *struct {
		Name       string `json:"name"`
		Parameters []struct {
			Name      string `json:"name"`
			Type      string `json:"type"`
			ValueType string `json:"valueType"`
			Value     string `json:"value"`
		} `json:"parameters"`
	}

	for _, group := range jsonResult.Groups {
		if group.Name == "default:2025-08-27" {
			defaultGroup = &group
			break
		}
	}

	if defaultGroup == nil {
		return fmt.Errorf("JSON output should contain default parameter group")
	}

	if len(defaultGroup.Parameters) < 5 { // We expect 7 parameters
		return fmt.Errorf("default parameter group should have at least 5 parameters, got %d", len(defaultGroup.Parameters))
	}

	// Test 4: Create test parameter file
	testParamFile := filepath.Join(h.dataDir, "test-params.json")
	// Use real PostgreSQL GUCs (not made-up names) so an instance created with
	// this group actually boots — create now waits for PostgreSQL readiness, so
	// a group of unrecognized parameters would (correctly) fail to start. The
	// three entries still exercise the numeric, numeric_mem-expression, and bool
	// value-type machinery.
	testParams := []map[string]any{
		{
			"name":      "max_connections",
			"type":      "postgres_cli_arg",
			"valueType": "numeric",
			"value":     "100",
		},
		{
			"name":      "shared_buffers",
			"type":      "postgres_cli_arg",
			"valueType": "numeric_mem",
			"value":     "{expr}round(DBContainerMemoryMB / 4){/expr} MB",
		},
		{
			"name":      "log_connections",
			"type":      "postgres_cli_arg",
			"valueType": "bool",
			"value":     "on",
		},
	}

	testParamsJSON, err := json.MarshalIndent(testParams, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal test parameters: %w", err)
	}

	if err := os.WriteFile(testParamFile, testParamsJSON, 0o600); err != nil {
		return fmt.Errorf("write test parameter file: %w", err)
	}

	// Test 5: Create custom parameter group
	createOutput, err := h.createParameterGroupCLI("test-group", testParamFile)
	if err != nil {
		return fmt.Errorf("create parameter group failed: %w", err)
	}

	if !strings.Contains(createOutput, "created successfully") {
		return fmt.Errorf("create output should indicate success: %s", createOutput)
	}

	// Test 6: Verify new group appears in list
	listOutput2, err := h.listParameterGroupsCLI()
	if err != nil {
		return fmt.Errorf("list parameter groups after create failed: %w", err)
	}

	if !strings.Contains(listOutput2, "test-group") {
		return fmt.Errorf("list output should contain new test-group: %s", listOutput2)
	}

	// Test 7: Get the new parameter group
	getTestOutput, err := h.getParameterGroupCLI("test-group")
	if err != nil {
		return fmt.Errorf("get test parameter group failed: %w", err)
	}

	if !strings.Contains(getTestOutput, "Parameter Group: test-group") {
		return fmt.Errorf("get test output should show parameter group name: %s", getTestOutput)
	}

	// Verify test parameters
	testParamNames := []string{"max_connections", "shared_buffers", "log_connections"}
	for _, param := range testParamNames {
		if !strings.Contains(getTestOutput, param) {
			return fmt.Errorf("get test output should contain parameter %s: %s", param, getTestOutput)
		}
	}

	// Test 8: Try to create parameter group with default: prefix (should fail)
	_, err = h.createParameterGroupCLI("default:custom", testParamFile)
	if err == nil {
		return fmt.Errorf("creating parameter group with 'default:' prefix should fail")
	}
	if !strings.Contains(err.Error(), "cannot create parameter groups with 'default:' prefix") {
		return fmt.Errorf("error should mention default prefix restriction: %v", err)
	}

	// Test 9: Try to delete current version's default parameter group (should fail)
	_, err = h.deleteParameterGroupCLI("default:2025-08-27", true)
	if err == nil {
		return fmt.Errorf("deleting current version's default parameter group should fail")
	}
	if !strings.Contains(err.Error(), "cannot delete the default parameter group of the current version") {
		return fmt.Errorf("error should mention current version protection: %v", err)
	}

	// Test 10: Try to delete parameter group that's in use (create an instance first)
	_, err = h.createInstanceWithParameterGroupCLI("temp-instance", 15999, "test-group")
	if err != nil {
		return fmt.Errorf("create instance for in-use test failed: %w", err)
	}

	// Try to delete parameter group while it's in use (should fail)
	_, err = h.deleteParameterGroupCLI("test-group", true)
	if err == nil {
		return fmt.Errorf("deleting parameter group in use should fail")
	}
	if !strings.Contains(err.Error(), "it is being used by") {
		return fmt.Errorf("error should mention parameter group is in use: %v", err)
	}

	if destroyErr := h.destroyInstanceCLI("temp-instance"); destroyErr != nil {
		return fmt.Errorf("cleanup test instance failed: %w", destroyErr)
	}

	// Test 11: Delete test parameter group (should work now)
	deleteOutput, err := h.deleteParameterGroupCLI("test-group", true)
	if err != nil {
		return fmt.Errorf("delete parameter group failed: %w", err)
	}

	if !strings.Contains(deleteOutput, "deleted successfully") {
		return fmt.Errorf("delete output should indicate success: %s", deleteOutput)
	}

	// Test 12: Verify group is gone from list
	listOutput3, err := h.listParameterGroupsCLI()
	if err != nil {
		return fmt.Errorf("list parameter groups after delete failed: %w", err)
	}

	if strings.Contains(listOutput3, "test-group") {
		return fmt.Errorf("list output should not contain deleted test-group: %s", listOutput3)
	}

	// Test 13: Try to get deleted group (should fail)
	_, err = h.getParameterGroupCLI("test-group")
	if err == nil {
		return fmt.Errorf("getting deleted parameter group should fail")
	}
	if !strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("error should indicate group not found: %v", err)
	}

	return nil
}

func testParameterGroupValidation(h *TestHarness) error {
	// Test 1: Invalid JSON format
	invalidJSONFile := filepath.Join(h.dataDir, "invalid.json")
	invalidJSON := `{"invalid": "json" "missing_comma": true}`
	if err := os.WriteFile(invalidJSONFile, []byte(invalidJSON), 0o600); err != nil {
		return fmt.Errorf("write invalid JSON file: %w", err)
	}

	_, err := h.createParameterGroupCLI("invalid-json", invalidJSONFile)
	if err == nil {
		return fmt.Errorf("creating parameter group with invalid JSON should fail")
	}

	// Test 2: Invalid parameter structure (missing required fields)
	incompleteParamFile := filepath.Join(h.dataDir, "incomplete.json")
	incompleteParams := []map[string]any{
		{
			"name": "incomplete_param",
			// Missing type, valueType, and value
		},
	}

	incompleteJSON, err := json.MarshalIndent(incompleteParams, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal incomplete parameters: %w", err)
	}

	if err := os.WriteFile(incompleteParamFile, incompleteJSON, 0o600); err != nil {
		return fmt.Errorf("write incomplete parameter file: %w", err)
	}

	_, err = h.createParameterGroupCLI("incomplete", incompleteParamFile)
	if err == nil {
		return fmt.Errorf("creating parameter group with incomplete parameters should fail")
	}
	if !strings.Contains(err.Error(), "required") {
		return fmt.Errorf("error should mention required fields: %v", err)
	}

	// Test 3: Invalid expression
	invalidExprFile := filepath.Join(h.dataDir, "invalid-expr.json")
	invalidExprParams := []map[string]any{
		{
			"name":      "bad_expr",
			"type":      "postgres_cli_arg",
			"valueType": "numeric",
			"value":     "{expr}invalid_syntax_here{/expr}",
		},
	}

	invalidExprJSON, err := json.MarshalIndent(invalidExprParams, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal invalid expression parameters: %w", err)
	}

	if err := os.WriteFile(invalidExprFile, invalidExprJSON, 0o600); err != nil {
		return fmt.Errorf("write invalid expression parameter file: %w", err)
	}

	_, err = h.createParameterGroupCLI("invalid-expr", invalidExprFile)
	if err == nil {
		return fmt.Errorf("creating parameter group with invalid expression should fail")
	}
	if !strings.Contains(err.Error(), "validation failed") {
		return fmt.Errorf("error should mention validation failure: %v", err)
	}

	// Test 4: Invalid value type validation
	invalidValueFile := filepath.Join(h.dataDir, "invalid-value.json")
	invalidValueParams := []map[string]any{
		{
			"name":      "bad_numeric_mem",
			"type":      "postgres_cli_arg",
			"valueType": "numeric_mem",
			"value":     "123", // Missing MB/GB suffix
		},
	}

	invalidValueJSON, err := json.MarshalIndent(invalidValueParams, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal invalid value parameters: %w", err)
	}

	if err := os.WriteFile(invalidValueFile, invalidValueJSON, 0o600); err != nil {
		return fmt.Errorf("write invalid value parameter file: %w", err)
	}

	_, err = h.createParameterGroupCLI("invalid-value", invalidValueFile)
	if err == nil {
		return fmt.Errorf("creating parameter group with invalid numeric_mem value should fail")
	}
	if !strings.Contains(err.Error(), "validation failed") {
		return fmt.Errorf("error should mention validation failure: %v", err)
	}

	return nil
}
