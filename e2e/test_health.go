package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Health API response structures
type HealthStatus struct {
	Overall      string        `json:"overall"`
	LastCheck    *HealthRecord `json:"lastCheck"`
	CheckRunning bool          `json:"checkRunning"`
	Timestamp    string        `json:"timestamp"`
}

type HealthRecord struct {
	ID               int64    `json:"id"`
	Timestamp        string   `json:"timestamp"`
	TimestampUnix    int64    `json:"timestampUnix"`
	InProgress       bool     `json:"inProgress"`
	HealthyAll       bool     `json:"healthyAll"`
	HealthyHost      bool     `json:"healthyHost"`
	HealthyInstances []string `json:"healthyInstances"`
	BrokenInstances  []string `json:"brokenInstances"`
	FailDetails      string   `json:"failDetails"`
}

type HealthHistory struct {
	Records   []*HealthRecord `json:"records"`
	Timestamp string          `json:"timestamp"`
}

// testHealthEndpoints tests the health monitoring API endpoints
// This must be the LAST test as it requires the daemon to have been running
// for a while to collect health data from previous tests
func testHealthEndpoints(h *TestHarness) error {
	// Check if we have any health data yet - if not, wait longer
	// This handles the case where someone runs only this test
	status, err := h.getHealthStatus()
	if err == nil && status.LastCheck == nil {
		fmt.Printf("⏳ No health data yet, waiting 5s for health checks to run...\n")
		time.Sleep(5 * time.Second)
	} else {
		// Wait a bit to ensure at least one health check has completed
		// since the daemon runs health checks every 2s in E2E mode
		time.Sleep(3 * time.Second)
	}

	// Test 1: Get current health status (refresh after potential wait)
	status, err = h.getHealthStatus()
	if err != nil {
		return fmt.Errorf("get health status: %w", err)
	}

	if status.Overall == "" {
		return fmt.Errorf("health status should have overall field")
	}

	if status.Timestamp == "" {
		return fmt.Errorf("health status should have timestamp")
	}

	// We should have at least one health check by now
	if status.LastCheck == nil {
		return fmt.Errorf("should have at least one health check record by now")
	}

	if status.LastCheck.ID == 0 {
		return fmt.Errorf("health record should have valid ID")
	}

	if status.LastCheck.TimestampUnix == 0 {
		return fmt.Errorf("health record should have valid timestamp")
	}

	// Host checks should generally be healthy in E2E environment
	// (Docker available, sufficient disk space)
	if !status.LastCheck.HealthyHost {
		fmt.Printf("⚠️  Warning: Host not healthy - this may be expected in some test environments")
		if status.LastCheck.FailDetails != "" {
			fmt.Printf("⚠️  Host health issue: %s", status.LastCheck.FailDetails)
		}
	}

	// Test 2: Get health history with default limit
	history, err := h.getHealthHistory(0) // 0 = use default limit
	if err != nil {
		return fmt.Errorf("get health history: %w", err)
	}

	if len(history.Records) == 0 {
		return fmt.Errorf("should have at least one health record in history")
	}

	// Verify records are in descending timestamp order
	for i := 1; i < len(history.Records); i++ {
		if history.Records[i].TimestampUnix > history.Records[i-1].TimestampUnix {
			return fmt.Errorf("health history should be in descending timestamp order")
		}
	}

	// Test 3: Get health history with custom limit
	limitedHistory, err := h.getHealthHistory(2)
	if err != nil {
		return fmt.Errorf("get health history with limit: %w", err)
	}

	if len(limitedHistory.Records) > 2 {
		return fmt.Errorf("limited history should respect limit parameter")
	}

	// Test 4: Test limit validation (should cap at reasonable maximum)
	largeHistory, err := h.getHealthHistory(1000) // Very large limit
	if err != nil {
		return fmt.Errorf("get health history with large limit: %w", err)
	}

	if len(largeHistory.Records) > 200 {
		return fmt.Errorf("health history should cap limit at maximum (200)")
	}

	fmt.Printf("✅ Health status: %s (last check: %s)\n",
		status.Overall,
		time.Unix(status.LastCheck.TimestampUnix, 0).Format("15:04:05"))

	fmt.Printf("✅ Health history contains %d records\n", len(history.Records))

	if status.LastCheck.HealthyHost {
		fmt.Printf("✅ Host health checks passing\n")
	}

	if len(status.LastCheck.HealthyInstances) > 0 {
		fmt.Printf("✅ Healthy instances: %v\n", status.LastCheck.HealthyInstances)
	}

	if len(status.LastCheck.BrokenInstances) > 0 {
		fmt.Printf("⚠️  Broken instances: %v\n", status.LastCheck.BrokenInstances)
	}

	return nil
}

// getHealthStatus fetches the current health status via API
func (h *TestHarness) getHealthStatus() (*HealthStatus, error) {
	req, err := http.NewRequest("GET", h.baseURL+"/api/health/status", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+h.authToken)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("make request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var status HealthStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &status, nil
}

// getHealthHistory fetches health history via API
func (h *TestHarness) getHealthHistory(limit int) (*HealthHistory, error) {
	url := h.baseURL + "/api/health/history"
	if limit > 0 {
		url = fmt.Sprintf("%s?limit=%d", url, limit)
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+h.authToken)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("make request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var history HealthHistory
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &history, nil
}
