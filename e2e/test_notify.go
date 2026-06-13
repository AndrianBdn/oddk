package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// WebhookServer represents a simple webhook server for testing
type WebhookServer struct {
	port     int
	server   *http.Server
	requests []WebhookRequest
	mu       sync.RWMutex
}

type WebhookRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Time    time.Time         `json:"time"`
}

func NewWebhookServer() *WebhookServer {
	return &WebhookServer{
		requests: make([]WebhookRequest, 0),
	}
}

func (w *WebhookServer) Start() error {
	mux := http.NewServeMux()

	// Catch all handler
	mux.HandleFunc("/", w.handleWebhook)

	// Use :0 to let the OS allocate a random available port
	// #nosec G102 - Binding to all interfaces is safe for test server
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to create listener: %w", err)
	}

	w.port = listener.Addr().(*net.TCPAddr).Port

	w.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second, // #nosec G112
	}

	go func() {
		if err := w.server.Serve(listener); err != http.ErrServerClosed {
			// Server error, but we can't do much here in test
			_ = err
		}
	}()

	// Give server a moment to start
	time.Sleep(10 * time.Millisecond)

	return nil
}

func (w *WebhookServer) handleWebhook(wr http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	// Capture request
	w.mu.Lock()
	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	w.requests = append(w.requests, WebhookRequest{
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: headers,
		Body:    string(body),
		Time:    time.Now(),
	})
	w.mu.Unlock()

	wr.WriteHeader(http.StatusOK)
	_, _ = wr.Write([]byte("OK"))
}

func (w *WebhookServer) GetRequests() []WebhookRequest {
	w.mu.RLock()
	defer w.mu.RUnlock()

	requests := make([]WebhookRequest, len(w.requests))
	copy(requests, w.requests)
	return requests
}

func (w *WebhookServer) ClearRequests() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.requests = w.requests[:0]
}

func (w *WebhookServer) Stop() {
	if w.server != nil {
		_ = w.server.Close()
	}
}

func (w *WebhookServer) URL(path string) string {
	return fmt.Sprintf("http://localhost:%d%s", w.port, path)
}

func NotificationOperations(h *TestHarness) error {
	webhook := NewWebhookServer()
	if err := webhook.Start(); err != nil {
		return fmt.Errorf("failed to start webhook server: %w", err)
	}
	defer webhook.Stop()

	fmt.Printf("Started webhook server on port %d\n", webhook.port)

	// Test 1: Check initial state - no notifications
	fmt.Printf("Checking initial state - no notifications...\n")

	output, err := h.runCLI("notify", "info")
	if err != nil {
		return fmt.Errorf("failed to get notification info: %w", err)
	}

	if !strings.Contains(output, "No notification configurations are active") {
		return fmt.Errorf("expected no notifications initially: %s", output)
	}

	fmt.Printf("Initial state confirmed - no notifications\n")

	// Test 2: Get template when no notifications exist
	fmt.Printf("Getting notification template...\n")

	output, err = h.runCLI("notify", "get")
	if err != nil {
		return fmt.Errorf("failed to get notification template: %w", err)
	}

	// Should return a template array
	var templateArray []map[string]any
	if err := json.Unmarshal([]byte(output), &templateArray); err != nil {
		return fmt.Errorf("failed to parse template JSON: %w", err)
	}

	if len(templateArray) == 0 {
		return fmt.Errorf("expected template array to have examples")
	}

	fmt.Printf("Template array retrieved successfully\n")

	// Test 3: Get help for webhook type
	fmt.Printf("Getting help for webhook notification type...\n")

	output, err = h.runCLI("notify", "help-add", "--type", "webhook")
	if err != nil {
		return fmt.Errorf("failed to get webhook help: %w", err)
	}

	var webhookHelp map[string]any
	if err := json.Unmarshal([]byte(output), &webhookHelp); err != nil {
		return fmt.Errorf("failed to parse webhook help JSON: %w", err)
	}

	if webhookHelp["type"] != "webhook" {
		return fmt.Errorf("expected webhook type in help")
	}

	fmt.Printf("Webhook help retrieved successfully\n")

	// Test 4: Create notifications configuration file
	fmt.Printf("Creating notifications configuration file...\n")

	notifications := []map[string]any{
		{
			"name": "test-webhook",
			"type": "webhook",
			"config": map[string]any{
				"url":    webhook.URL("/notifications"),
				"method": "POST",
				"headers": map[string]string{
					"Content-Type":  "application/json",
					"Authorization": "Bearer test-token",
				},
				"requestBodyType":       "json",
				"requestBodyMessageKey": "message",
			},
		},
		{
			"name": "slack-notification",
			"type": "slack",
			"config": map[string]any{
				"slackWebhookUrl": webhook.URL("/slack"),
			},
		},
	}

	configFile := "notifications.json"
	notifData, err := json.MarshalIndent(notifications, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal notifications: %w", err)
	}

	if err := os.WriteFile(configFile, notifData, 0o600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	defer func() { _ = os.Remove(configFile) }()

	fmt.Printf("Configuration file created with 2 notifications\n")

	// Test 5: Apply notifications from file
	fmt.Printf("Applying notifications from file...\n")

	output, err = h.runCLI("notify", "apply", "--file", configFile)
	if err != nil {
		return fmt.Errorf("failed to apply notifications: %w", err)
	}

	if !strings.Contains(output, "Notification configuration applied successfully") {
		return fmt.Errorf("apply output unexpected: %s", output)
	}

	if !strings.Contains(output, "test-webhook") || !strings.Contains(output, "slack-notification") {
		return fmt.Errorf("expected both notifications in output: %s", output)
	}

	fmt.Printf("Notifications applied successfully\n")

	// Test 6: Check notification info
	fmt.Printf("Checking notification info...\n")

	output, err = h.runCLI("notify", "info")
	if err != nil {
		return fmt.Errorf("failed to get notification info: %w", err)
	}

	if !strings.Contains(output, "Active Notifications: 2") {
		return fmt.Errorf("expected 2 active notifications: %s", output)
	}

	if !strings.Contains(output, "test-webhook") || !strings.Contains(output, "slack-notification") {
		return fmt.Errorf("expected both notifications in info: %s", output)
	}

	fmt.Printf("Notification info shows both notifications correctly\n")

	// Test 7: Test notifications (send webhooks)
	fmt.Printf("Testing notifications...\n")
	webhook.ClearRequests()

	_, err = h.runCLI("notify", "test")
	if err != nil {
		return fmt.Errorf("failed to test notifications: %w", err)
	}

	// Wait for webhooks to be received
	time.Sleep(500 * time.Millisecond)

	requests := webhook.GetRequests()
	if len(requests) < 2 {
		return fmt.Errorf("expected 2 webhook requests, got %d", len(requests))
	}

	var webhookReq, slackReq *WebhookRequest
	for i := range requests {
		switch requests[i].Path {
		case "/notifications":
			webhookReq = &requests[i]
		case "/slack":
			slackReq = &requests[i]
		}
	}

	if webhookReq == nil {
		return fmt.Errorf("webhook notification request not found")
	}

	if webhookReq.Method != "POST" {
		return fmt.Errorf("expected POST request, got %s", webhookReq.Method)
	}

	if webhookReq.Headers["Content-Type"] != "application/json" {
		return fmt.Errorf("expected Content-Type application/json, got %s", webhookReq.Headers["Content-Type"])
	}

	if webhookReq.Headers["Authorization"] != "Bearer test-token" {
		return fmt.Errorf("expected Authorization header, got %s", webhookReq.Headers["Authorization"])
	}

	var webhookBody map[string]any
	if err := json.Unmarshal([]byte(webhookReq.Body), &webhookBody); err != nil {
		return fmt.Errorf("failed to parse webhook JSON body: %w", err)
	}

	if msg, ok := webhookBody["message"]; !ok || !strings.Contains(fmt.Sprintf("%v", msg), "ODDK Test Notification") {
		return fmt.Errorf("expected test message in webhook body, got: %s", webhookReq.Body)
	}

	if slackReq == nil {
		return fmt.Errorf("slack notification request not found")
	}

	var slackBody map[string]any
	if err := json.Unmarshal([]byte(slackReq.Body), &slackBody); err != nil {
		return fmt.Errorf("failed to parse Slack JSON body: %w", err)
	}

	if text, ok := slackBody["text"]; !ok || !strings.Contains(fmt.Sprintf("%v", text), "ODDK Test Notification") {
		return fmt.Errorf("expected test message in Slack 'text' field, got: %s", slackReq.Body)
	}

	fmt.Printf("Both notifications sent successfully with correct formats\n")

	// Test 8: Get current configuration
	fmt.Printf("Getting current notification configuration...\n")

	output, err = h.runCLI("notify", "get")
	if err != nil {
		return fmt.Errorf("failed to get notifications: %w", err)
	}

	var currentNotifications []map[string]any
	if err := json.Unmarshal([]byte(output), &currentNotifications); err != nil {
		return fmt.Errorf("failed to parse notifications JSON: %w", err)
	}

	if len(currentNotifications) != 2 {
		return fmt.Errorf("expected 2 notifications, got %d", len(currentNotifications))
	}

	// Verify both notifications are present
	hasWebhook := false
	hasSlack := false
	for _, n := range currentNotifications {
		if n["name"] == "test-webhook" && n["type"] == "webhook" {
			hasWebhook = true
		}
		if n["name"] == "slack-notification" && n["type"] == "slack" {
			hasSlack = true
		}
	}

	if !hasWebhook || !hasSlack {
		return fmt.Errorf("expected both webhook and slack notifications")
	}

	fmt.Printf("Current configuration retrieved correctly\n")

	// Test 9: Update notifications (change webhook path)
	fmt.Printf("Updating notifications to change webhook path...\n")

	notifications[0]["config"].(map[string]any)["url"] = webhook.URL("/updated-path")
	updatedNotifData, err := json.MarshalIndent(notifications, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal updated notifications: %w", err)
	}

	if err := os.WriteFile(configFile, updatedNotifData, 0o600); err != nil {
		return fmt.Errorf("failed to write updated config file: %w", err)
	}

	output, err = h.runCLI("notify", "apply", "--file", configFile)
	if err != nil {
		return fmt.Errorf("failed to apply updated notifications: %w", err)
	}

	if !strings.Contains(output, "Notification configuration applied successfully") {
		return fmt.Errorf("update output unexpected: %s", output)
	}

	if !strings.Contains(output, "Updated") {
		return fmt.Errorf("expected 'Updated' in output: %s", output)
	}

	fmt.Printf("Notifications updated successfully\n")

	// Test 10: Test updated notification
	fmt.Printf("Testing updated notification...\n")
	webhook.ClearRequests()

	_, err = h.runCLI("notify", "test")
	if err != nil {
		return fmt.Errorf("failed to test updated notifications: %w", err)
	}

	time.Sleep(500 * time.Millisecond)

	requests = webhook.GetRequests()
	if len(requests) < 2 {
		return fmt.Errorf("expected 2 webhook requests after update, got %d", len(requests))
	}

	var updatedWebhookReq *WebhookRequest
	for i := range requests {
		if requests[i].Path == "/updated-path" {
			updatedWebhookReq = &requests[i]
			break
		}
	}

	if updatedWebhookReq == nil {
		return fmt.Errorf("updated webhook request not found")
	}

	fmt.Printf("Updated notification sent to correct path: %s\n", updatedWebhookReq.Path)

	// Test 11: Test invalid notification type in help-add
	fmt.Printf("Testing invalid notification type...\n")

	_, err = h.runCLI("notify", "help-add", "--type", "invalid-type")
	if err == nil {
		return fmt.Errorf("expected error for invalid type, but command succeeded")
	}

	if !strings.Contains(err.Error(), "invalid notification type") {
		return fmt.Errorf("expected invalid type error, got: %s", err.Error())
	}

	fmt.Printf("Invalid notification type validation working correctly\n")

	// Test 12: Test duplicate names in apply
	fmt.Printf("Testing duplicate names in configuration...\n")

	duplicateNotifications := []map[string]any{
		{
			"name": "duplicate-name",
			"type": "webhook",
			"config": map[string]any{
				"url": webhook.URL("/test1"),
			},
		},
		{
			"name": "duplicate-name",
			"type": "slack",
			"config": map[string]any{
				"slackWebhookUrl": webhook.URL("/test2"),
			},
		},
	}

	duplicateFile := "duplicate-notifications.json"
	duplicateData, err := json.MarshalIndent(duplicateNotifications, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal duplicate notifications: %w", err)
	}

	if err := os.WriteFile(duplicateFile, duplicateData, 0o600); err != nil {
		return fmt.Errorf("failed to write duplicate file: %w", err)
	}
	defer func() { _ = os.Remove(duplicateFile) }()

	_, err = h.runCLI("notify", "apply", "--file", duplicateFile)
	if err == nil {
		return fmt.Errorf("expected error for duplicate names, but command succeeded")
	}

	if !strings.Contains(err.Error(), "duplicate notification name") {
		return fmt.Errorf("expected duplicate name error, got: %s", err.Error())
	}

	fmt.Printf("Duplicate name validation working correctly\n")

	// Test 13: Remove all notifications
	fmt.Printf("Removing all notifications...\n")

	output, err = h.runCLI("notify", "remove", "--force")
	if err != nil {
		return fmt.Errorf("failed to remove all notifications: %w", err)
	}

	if !strings.Contains(output, "All 2 notification(s) removed successfully") {
		return fmt.Errorf("remove all output unexpected: %s", output)
	}

	fmt.Printf("All notifications removed successfully\n")

	// Test 14: Verify all notifications are deleted
	fmt.Printf("Verifying all notifications are deleted...\n")

	output, err = h.runCLI("notify", "info")
	if err != nil {
		return fmt.Errorf("failed to get info after removal: %w", err)
	}

	if !strings.Contains(output, "No notification configurations are active") {
		return fmt.Errorf("expected no notifications after removal: %s", output)
	}

	fmt.Printf("All notifications successfully removed\n")

	// Test 15: Try to remove when no notifications exist
	fmt.Printf("Testing remove when no notifications exist...\n")

	output, err = h.runCLI("notify", "remove", "--force")
	if err != nil {
		return fmt.Errorf("failed to remove when no notifications: %w", err)
	}

	if !strings.Contains(output, "No notifications to remove") {
		return fmt.Errorf("expected 'No notifications to remove': %s", output)
	}

	fmt.Printf("Remove gracefully handles no notifications\n")

	// Test 16: Test notification logs
	logsOutput, err := h.notifyLogsCLI()
	if err != nil {
		return fmt.Errorf("notification logs failed: %w", err)
	}

	// Should handle empty logs gracefully (or show logs if any exist from previous test notifications)
	if !strings.Contains(logsOutput, "No notification logs found") && !strings.Contains(logsOutput, "ID") {
		return fmt.Errorf("logs output should show either no logs or table headers: %s", logsOutput)
	}

	fmt.Printf("Notification logs command working correctly\n")

	// Test 17: Test system display name in notifications
	fmt.Printf("Testing system display name in notifications...\n")

	displayNameNotifications := []map[string]any{
		{
			"name": "display-name-test",
			"type": "webhook",
			"config": map[string]any{
				"url":                   webhook.URL("/display-test"),
				"method":                "POST",
				"headers":               map[string]string{"Content-Type": "application/json"},
				"requestBodyType":       "json",
				"requestBodyMessageKey": "message",
			},
		},
	}

	displayNameFile := "display-name-notifications.json"
	displayNameData, err := json.MarshalIndent(displayNameNotifications, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal display name notifications: %w", err)
	}

	if err := os.WriteFile(displayNameFile, displayNameData, 0o600); err != nil {
		return fmt.Errorf("failed to write display name config file: %w", err)
	}
	defer func() { _ = os.Remove(displayNameFile) }()

	_, err = h.runCLI("notify", "apply", "--file", displayNameFile)
	if err != nil {
		return fmt.Errorf("failed to apply display name notification: %w", err)
	}

	// Set custom display name
	fmt.Printf("Setting custom display name...\n")
	_, err = h.runCLI("customkv", "set", "system.display.name.str", "--value", "Test-Server-Production")
	if err != nil {
		return fmt.Errorf("failed to set display name: %w", err)
	}

	// Clear requests and send test notification
	webhook.ClearRequests()
	_, err = h.runCLI("notify", "test")
	if err != nil {
		return fmt.Errorf("failed to send test notification with display name: %w", err)
	}

	time.Sleep(500 * time.Millisecond)

	requests = webhook.GetRequests()
	if len(requests) < 1 {
		return fmt.Errorf("expected at least 1 webhook request, got %d", len(requests))
	}

	// Find the display-test request
	var displayTestReq *WebhookRequest
	for i := range requests {
		if requests[i].Path == "/display-test" {
			displayTestReq = &requests[i]
			break
		}
	}

	if displayTestReq == nil {
		return fmt.Errorf("display-test webhook request not found")
	}

	var displayTestBody map[string]any
	if err := json.Unmarshal([]byte(displayTestReq.Body), &displayTestBody); err != nil {
		return fmt.Errorf("failed to parse display-test JSON body: %w", err)
	}

	messageText := fmt.Sprintf("%v", displayTestBody["message"])
	if !strings.Contains(messageText, "Test-Server-Production") {
		return fmt.Errorf("expected display name 'Test-Server-Production' in message, got: %s", messageText)
	}

	fmt.Printf("Display name 'Test-Server-Production' found in notification message\n")

	// Test with empty display name (should use hostname)
	fmt.Printf("Testing with empty display name (hostname fallback)...\n")
	_, err = h.runCLI("customkv", "set", "system.display.name.str", "--value", "")
	if err != nil {
		return fmt.Errorf("failed to clear display name: %w", err)
	}

	webhook.ClearRequests()
	_, err = h.runCLI("notify", "test")
	if err != nil {
		return fmt.Errorf("failed to send test notification with hostname: %w", err)
	}

	time.Sleep(500 * time.Millisecond)

	requests = webhook.GetRequests()
	if len(requests) < 1 {
		return fmt.Errorf("expected at least 1 webhook request with hostname, got %d", len(requests))
	}

	// Find the display-test request
	var hostnameTestReq *WebhookRequest
	for i := range requests {
		if requests[i].Path == "/display-test" {
			hostnameTestReq = &requests[i]
			break
		}
	}

	if hostnameTestReq == nil {
		return fmt.Errorf("hostname-test webhook request not found")
	}

	var hostnameTestBody map[string]any
	if err := json.Unmarshal([]byte(hostnameTestReq.Body), &hostnameTestBody); err != nil {
		return fmt.Errorf("failed to parse hostname-test JSON body: %w", err)
	}

	hostnameMessageText := fmt.Sprintf("%v", hostnameTestBody["message"])
	// Should contain ODDK Test Notification but not be "Test-Server-Production"
	if !strings.Contains(hostnameMessageText, "ODDK Test Notification") {
		return fmt.Errorf("expected ODDK Test Notification in message, got: %s", hostnameMessageText)
	}
	if strings.Contains(hostnameMessageText, "Test-Server-Production") {
		return fmt.Errorf("should not contain custom display name after clearing, got: %s", hostnameMessageText)
	}

	// Should contain hostname in parentheses
	if !strings.Contains(hostnameMessageText, "(") || !strings.Contains(hostnameMessageText, ")") {
		return fmt.Errorf("expected hostname in parentheses in message, got: %s", hostnameMessageText)
	}

	fmt.Printf("Hostname fallback working correctly in notification\n")

	_, err = h.runCLI("notify", "remove", "--force")
	if err != nil {
		return fmt.Errorf("failed to remove display name notification: %w", err)
	}

	fmt.Printf("Display name test completed successfully\n")

	fmt.Printf("All notification operations completed successfully!\n")
	return nil
}

// CLI helper function for notification logs
func (h *TestHarness) notifyLogsCLI() (string, error) {
	return h.runCLI("notify", "logs")
}
