package main

import (
	"fmt"
	"strings"
	"time"
)

// testHealthDebugFail tests the health check debug fail feature
func testHealthDebugFail(h *TestHarness) error {
	// Wait a moment for initial health checks to run
	time.Sleep(3 * time.Second)

	// Test 1: Get initial health status (should be healthy)
	initialStatus, err := h.getHealthStatus()
	if err != nil {
		return fmt.Errorf("get initial health status: %w", err)
	}

	if initialStatus.LastCheck == nil {
		return fmt.Errorf("should have at least one health check by now")
	}

	initialHealthy := initialStatus.LastCheck.HealthyHost
	fmt.Printf("Initial health status - Host healthy: %v\n", initialHealthy)

	// Test 2: Enable debug fail mode
	setOutput, err := h.customKVSetCLI("health.debug_fail.int", "1")
	if err != nil {
		return fmt.Errorf("enable debug fail: %w", err)
	}

	if !strings.Contains(setOutput, "Set health.debug_fail.int = 1") {
		return fmt.Errorf("unexpected set output: %s", setOutput)
	}

	// Test 3: Wait for next health check cycle (E2E tests use 2s interval)
	fmt.Printf("Waiting for health check with debug fail enabled...\n")
	time.Sleep(3 * time.Second)

	// Test 4: Verify health check now reports failure
	failedStatus, err := h.getHealthStatus()
	if err != nil {
		return fmt.Errorf("get failed health status: %w", err)
	}

	if failedStatus.LastCheck == nil {
		return fmt.Errorf("should have health check after enabling debug fail")
	}

	if failedStatus.LastCheck.HealthyHost {
		return fmt.Errorf("host should be unhealthy with debug fail enabled")
	}

	if !strings.Contains(failedStatus.LastCheck.FailDetails, "debug_fail_enabled") {
		return fmt.Errorf("fail details should indicate debug fail: %s", failedStatus.LastCheck.FailDetails)
	}

	fmt.Printf("✅ Health check failed as expected with debug fail enabled\n")
	fmt.Printf("   Fail details: %s\n", failedStatus.LastCheck.FailDetails)

	// Test 5: Disable debug fail mode
	setOutput2, err := h.customKVSetCLI("health.debug_fail.int", "0")
	if err != nil {
		return fmt.Errorf("disable debug fail: %w", err)
	}

	if !strings.Contains(setOutput2, "Set health.debug_fail.int = 0") {
		return fmt.Errorf("unexpected set output: %s", setOutput2)
	}

	// Test 6: Wait for recovery
	fmt.Printf("Waiting for health check recovery...\n")
	time.Sleep(3 * time.Second)

	// Test 7: Verify health check recovers
	recoveredStatus, err := h.getHealthStatus()
	if err != nil {
		return fmt.Errorf("get recovered health status: %w", err)
	}

	if recoveredStatus.LastCheck == nil {
		return fmt.Errorf("should have health check after disabling debug fail")
	}

	if !recoveredStatus.LastCheck.HealthyHost {
		// This might be a legitimate failure, so just warn
		fmt.Printf("⚠️  Warning: Host still unhealthy after disabling debug fail\n")
		fmt.Printf("   Fail details: %s\n", recoveredStatus.LastCheck.FailDetails)
		if strings.Contains(recoveredStatus.LastCheck.FailDetails, "debug_fail_enabled") {
			return fmt.Errorf("debug fail should be disabled but still appears in fail details")
		}
	} else {
		fmt.Printf("✅ Health check recovered after disabling debug fail\n")
	}

	// Test 8: Verify we can query the debug fail setting
	getOutput, err := h.customKVGetCLI("health.debug_fail.int")
	if err != nil {
		return fmt.Errorf("get debug fail value: %w", err)
	}

	if !strings.Contains(getOutput, "Value:   0") {
		return fmt.Errorf("debug fail should be disabled (0): %s", getOutput)
	}

	fmt.Printf("✅ Debug fail setting verified as disabled\n")

	return nil
}
