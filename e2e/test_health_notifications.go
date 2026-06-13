package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// testHealthNotifications tests the health notification system end-to-end
func testHealthNotifications(h *TestHarness) error {
	webhookServer := NewWebhookServer()
	if err := webhookServer.Start(); err != nil {
		return fmt.Errorf("failed to start webhook server: %w", err)
	}
	defer webhookServer.Stop()

	webhookURL := webhookServer.URL("/health-alerts")
	fmt.Printf("Started webhook server at: %s\n", webhookURL)

	notifications := []map[string]any{
		{
			"name": "health-alerts",
			"type": "webhook",
			"config": map[string]any{
				"url":    webhookURL,
				"method": "POST",
				"headers": map[string]string{
					"Content-Type":  "application/json",
					"X-Test-Source": "health-notification-test",
				},
			},
		},
	}

	configFile := "health-alerts.json"
	configData, err := json.MarshalIndent(notifications, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal notifications: %w", err)
	}

	if err := os.WriteFile(configFile, configData, 0o600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	defer func() { _ = os.Remove(configFile) }() // cleanup

	// Apply notifications via CLI
	_, err = h.runCLI("notify", "apply", "--file", configFile)
	if err != nil {
		return fmt.Errorf("failed to add notification: %w", err)
	}
	fmt.Printf("✅ Added health notification configuration\n")

	_, err = h.customKVSetCLI("health.degraded_threshold.int", "2")
	if err != nil {
		return fmt.Errorf("failed to set degraded threshold: %w", err)
	}
	fmt.Printf("✅ Set health degraded threshold to 2\n")

	fmt.Printf("⏳ Waiting 3s for initial healthy state...\n")
	time.Sleep(3 * time.Second)
	webhookServer.ClearRequests()

	_, err = h.customKVSetCLI("health.debug_fail.int", "1")
	if err != nil {
		return fmt.Errorf("failed to enable debug fail: %w", err)
	}
	fmt.Printf("✅ Enabled debug fail mode\n")

	// With 2s health check interval and threshold of 2, we need ~4-6s for the degraded notification.
	fmt.Printf("⏳ Waiting 8s for degraded notification...\n")
	time.Sleep(8 * time.Second)

	requests := webhookServer.GetRequests()
	fmt.Printf("📧 Received %d webhook requests\n", len(requests))

	var degradedNotification *WebhookRequest
	for i := range requests {
		req := &requests[i]
		fmt.Printf("   Request %d: %s %s - %s\n", i+1, req.Method, req.Path, req.Body[:min(50, len(req.Body))])

		bodyLower := strings.ToLower(req.Body)
		if strings.Contains(bodyLower, "degraded") {
			degradedNotification = req
		}
	}

	if degradedNotification == nil {
		return fmt.Errorf("no degraded notification received - got %d requests", len(requests))
	}

	// Verify the degraded notification content (case-insensitive)
	bodyLower := strings.ToLower(degradedNotification.Body)
	if !strings.Contains(bodyLower, "oddk") || !strings.Contains(bodyLower, "degraded") {
		return fmt.Errorf("degraded notification should mention ODDK and degraded: %s", degradedNotification.Body)
	}

	if !strings.Contains(bodyLower, "debug_fail_enabled") {
		return fmt.Errorf("degraded notification should contain debug_fail_enabled: %s", degradedNotification.Body)
	}

	// Verify headers
	if degradedNotification.Headers["X-Test-Source"] != "health-notification-test" {
		return fmt.Errorf("expected custom header not found: %v", degradedNotification.Headers)
	}

	fmt.Printf("✅ Received degraded notification with correct content\n")

	webhookServer.ClearRequests()
	_, err = h.customKVSetCLI("health.debug_fail.int", "0")
	if err != nil {
		return fmt.Errorf("failed to disable debug fail: %w", err)
	}
	fmt.Printf("✅ Disabled debug fail mode\n")

	// With threshold of 2 successes, we need ~4-6s for the restored notification.
	fmt.Printf("⏳ Waiting 8s for restored notification...\n")
	time.Sleep(8 * time.Second)

	restoredRequests := webhookServer.GetRequests()
	fmt.Printf("📧 Received %d webhook requests after recovery\n", len(restoredRequests))

	var restoredNotification *WebhookRequest
	for i := range restoredRequests {
		req := &restoredRequests[i]
		fmt.Printf("   Request %d: %s %s - %s\n", i+1, req.Method, req.Path, req.Body[:min(50, len(req.Body))])

		bodyLower := strings.ToLower(req.Body)
		if strings.Contains(bodyLower, "restored") {
			restoredNotification = req
		}
	}

	if restoredNotification == nil {
		return fmt.Errorf("no restored notification received - got %d requests", len(restoredRequests))
	}

	// Verify the restored notification content (case-insensitive)
	restoredBodyLower := strings.ToLower(restoredNotification.Body)
	if !strings.Contains(restoredBodyLower, "oddk") || !strings.Contains(restoredBodyLower, "restored") {
		return fmt.Errorf("restored notification should mention ODDK and restored: %s", restoredNotification.Body)
	}

	fmt.Printf("✅ Received restored notification with correct content\n")

	_, err = h.runCLI("notify", "remove", "--force")
	if err != nil {
		return fmt.Errorf("failed to remove notification: %w", err)
	}
	fmt.Printf("✅ Cleaned up notification configuration\n")

	fmt.Printf("🎉 Health notification system working correctly!\n")
	return nil
}
