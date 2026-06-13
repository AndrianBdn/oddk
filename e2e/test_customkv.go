package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// KVPair represents a key-value pair for API responses
type KVPair struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updatedAt"`
}

func testCustomKVOperations(h *TestHarness) error {
	// Test 1: List initial KV pairs (should have system defaults)
	listOutput, err := h.customKVListCLI()
	if err != nil {
		return fmt.Errorf("list custom KV pairs failed: %w", err)
	}

	// Should have system default keys
	if !strings.Contains(listOutput, "health.degraded_threshold.int") {
		return fmt.Errorf("should have system defaults in list: %s", listOutput)
	}

	// Test 2: Set existing system parameter (integer)
	testKey := "health.debug_fail.int"
	testValue := "1" // Enable debug fail mode
	setOutput, err := h.customKVSetCLI(testKey, testValue)
	if err != nil {
		return fmt.Errorf("set integer KV pair failed: %w", err)
	}

	if !strings.Contains(setOutput, fmt.Sprintf("Set %s = %s", testKey, testValue)) {
		return fmt.Errorf("set output should confirm the operation: %s", setOutput)
	}

	// Test 3: Set another existing system parameter (integer)
	intKey := "health.degraded_threshold.int"
	intValue := "5"
	setIntOutput, err := h.customKVSetCLI(intKey, intValue)
	if err != nil {
		return fmt.Errorf("set integer KV pair failed: %w", err)
	}

	if !strings.Contains(setIntOutput, fmt.Sprintf("Set %s = %s", intKey, intValue)) {
		return fmt.Errorf("set int output should confirm the operation: %s", setIntOutput)
	}

	// Test 4: Get specific key-value pair
	getOutput, err := h.customKVGetCLI(testKey)
	if err != nil {
		return fmt.Errorf("get KV pair failed: %w", err)
	}

	if !strings.Contains(getOutput, fmt.Sprintf("Key:     %s", testKey)) {
		return fmt.Errorf("get output should show key: %s", getOutput)
	}
	if !strings.Contains(getOutput, fmt.Sprintf("Value:   %s", testValue)) {
		return fmt.Errorf("get output should show value: %s", getOutput)
	}
	if !strings.Contains(getOutput, "Updated:") {
		return fmt.Errorf("get output should show timestamp: %s", getOutput)
	}

	// Test 5: List non-empty custom KV pairs
	listOutput2, err := h.customKVListCLI()
	if err != nil {
		return fmt.Errorf("list custom KV pairs after adding failed: %w", err)
	}

	// Should show table headers
	if !strings.Contains(listOutput2, "KEY") || !strings.Contains(listOutput2, "VALUE") || !strings.Contains(listOutput2, "UPDATED") {
		return fmt.Errorf("list output should have table headers: %s", listOutput2)
	}

	// Should show both keys
	if !strings.Contains(listOutput2, testKey) || !strings.Contains(listOutput2, intKey) {
		return fmt.Errorf("list output should show both keys: %s", listOutput2)
	}

	if !strings.Contains(listOutput2, testValue) || !strings.Contains(listOutput2, intValue) {
		return fmt.Errorf("list output should show both values: %s", listOutput2)
	}

	// Test 6: Try to set non-existent key via API (should fail now)
	invalidKey := "non.existent.int"
	_, err = h.customKVSetAPI(invalidKey, "123")
	if err == nil {
		return fmt.Errorf("setting non-existent key via API should have failed")
	}
	if !strings.Contains(err.Error(), "key not found") {
		return fmt.Errorf("non-existent key error should mention key not found: %s", err.Error())
	}

	// Test 7: Try to set invalid integer value on existing key
	_, err = h.customKVSetCLI("health.debug_fail.int", "not_a_number")
	if err == nil {
		return fmt.Errorf("setting invalid integer value should have failed")
	}
	if !strings.Contains(err.Error(), "must be a valid integer") {
		return fmt.Errorf("invalid integer error should mention validation: %s", err.Error())
	}

	// Test 8: Update existing key (reset debug_fail back to 0)
	newValue := "0" // Disable debug fail mode
	updateOutput, err := h.customKVSetCLI(testKey, newValue)
	if err != nil {
		return fmt.Errorf("update KV pair failed: %w", err)
	}

	if !strings.Contains(updateOutput, fmt.Sprintf("Set %s = %s", testKey, newValue)) {
		return fmt.Errorf("update output should confirm the operation: %s", updateOutput)
	}

	// Verify the update
	getUpdatedOutput, err := h.customKVGetCLI(testKey)
	if err != nil {
		return fmt.Errorf("get updated KV pair failed: %w", err)
	}

	if !strings.Contains(getUpdatedOutput, fmt.Sprintf("Value:   %s", newValue)) {
		return fmt.Errorf("get output should show updated value: %s", getUpdatedOutput)
	}

	// Delete command has been removed from CLI, skipping delete tests
	// Keys will persist but won't affect other tests

	return nil
}

// CLI helper functions for custom KV operations
func (h *TestHarness) customKVListCLI() (string, error) {
	return h.runCLI("customkv", "list")
}

func (h *TestHarness) customKVGetCLI(key string) (string, error) {
	return h.runCLI("customkv", "get", key)
}

func (h *TestHarness) customKVSetCLI(key, value string) (string, error) {
	return h.runCLI("customkv", "set", key, "--value", value)
}

// API helper functions for custom KV operations
func (h *TestHarness) customKVListAPI() ([]KVPair, error) {
	status, body, err := h.request("GET", "/api/customkv", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("list KV API returned status %d: %s", status, string(body))
	}

	var pairs []KVPair
	if err := json.Unmarshal(body, &pairs); err != nil {
		return nil, fmt.Errorf("parse KV list response: %w", err)
	}

	return pairs, nil
}

func (h *TestHarness) customKVGetAPI(key string) (*KVPair, error) {
	path := fmt.Sprintf("/api/customkv/%s", key)
	status, body, err := h.request("GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("get KV API returned status %d: %s", status, string(body))
	}

	var pair KVPair
	if err := json.Unmarshal(body, &pair); err != nil {
		return nil, fmt.Errorf("parse KV get response: %w", err)
	}

	return &pair, nil
}

func (h *TestHarness) customKVSetAPI(key, value string) (*KVPair, error) {
	path := fmt.Sprintf("/api/customkv/%s", key)
	request := map[string]string{"value": value}

	status, body, err := h.request("PUT", path, request)
	if err != nil {
		return nil, err
	}
	if status != 201 {
		return nil, fmt.Errorf("set KV API returned status %d: %s", status, string(body))
	}

	var pair KVPair
	if err := json.Unmarshal(body, &pair); err != nil {
		return nil, fmt.Errorf("parse KV set response: %w", err)
	}

	return &pair, nil
}

func testCustomKVAPIOperations(h *TestHarness) error {
	// Test API operations directly

	// Test 1: List initial KV pairs via API (should have system defaults)
	pairs, err := h.customKVListAPI()
	if err != nil {
		return fmt.Errorf("API list KV pairs failed: %w", err)
	}

	// Should have system default keys (at least 4, but could be more)
	if len(pairs) < 4 {
		return fmt.Errorf("expected at least 4 system default pairs, got %d pairs", len(pairs))
	}

	// Test 2: Set key-value via API (using existing system parameter)
	testKey := "health.cpu_load_threshold_percent.int"
	testValue := "90" // Set CPU threshold to 90%

	setPair, err := h.customKVSetAPI(testKey, testValue)
	if err != nil {
		return fmt.Errorf("API set KV pair failed: %w", err)
	}

	if setPair.Key != testKey || setPair.Value != testValue {
		return fmt.Errorf("set response mismatch: got %+v", setPair)
	}

	if setPair.UpdatedAt == "" {
		return fmt.Errorf("set response should include UpdatedAt timestamp")
	}

	// Verify timestamp format
	if _, err := time.Parse("2006-01-02T15:04:05Z07:00", setPair.UpdatedAt); err != nil {
		return fmt.Errorf("invalid UpdatedAt timestamp format: %s", setPair.UpdatedAt)
	}

	// Test 3: Get key-value via API
	getPair, err := h.customKVGetAPI(testKey)
	if err != nil {
		return fmt.Errorf("API get KV pair failed: %w", err)
	}

	if getPair.Key != testKey || getPair.Value != testValue {
		return fmt.Errorf("get response mismatch: got %+v", getPair)
	}

	// Test 4: List KV pairs via API (should still have 6 system defaults since we only modified existing ones)
	pairs2, err := h.customKVListAPI()
	if err != nil {
		return fmt.Errorf("API list KV pairs after modifying failed: %w", err)
	}

	if len(pairs2) < 4 {
		return fmt.Errorf("expected at least 4 KV pairs (system defaults), got %d", len(pairs2))
	}

	// Find our test key in the list
	found := false
	for _, pair := range pairs2 {
		if pair.Key == testKey {
			if pair.Value != testValue {
				return fmt.Errorf("list response value mismatch: got %s", pair.Value)
			}
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("test key not found in list")
	}

	// Delete API endpoint has been removed, skipping delete tests
	// Custom keys will persist along with system defaults

	return nil
}
