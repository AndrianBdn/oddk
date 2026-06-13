package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/hypersequent/oddk/internal/operations"
	"github.com/hypersequent/oddk/internal/store/notifications"
)

func (c *Client) notifyInfoAction(ctx context.Context, cmd *cli.Command) error {
	resp, err := c.request("GET", "/api/notifications", nil)
	if err != nil {
		return err
	}

	var result operations.NotificationListResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if len(result.Notifications) == 0 {
		_, _ = fmt.Fprintln(c.out, "No notification configurations are active")
		return nil
	}

	_, _ = fmt.Fprintf(c.out, "Active Notifications: %d\n\n", len(result.Notifications))

	headers := []string{"NAME", "TYPE", "CREATED", "UPDATED"}
	var rows [][]string

	for _, n := range result.Notifications {
		created := n.CreatedAt.Format("2006-01-02")
		updated := n.UpdatedAt.Format("2006-01-02")
		rows = append(rows, []string{n.Name, string(n.Type), created, updated})
	}

	return writeTable(c.out, headers, rows)
}

func (c *Client) notifyGetAction(ctx context.Context, cmd *cli.Command) error {
	resp, err := c.request("GET", "/api/notifications", nil)
	if err != nil {
		return err
	}

	var result operations.NotificationListResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if len(result.Notifications) == 0 {
		// Return a template array if no configurations exist
		// Use server template endpoint to ensure correct schema
		var templates []notifications.Notification

		// Get email template
		emailResp, err := c.request("GET", "/api/notifications/oddk:template:email", nil)
		if err == nil {
			var emailResult operations.NotificationGetResult
			if err := json.Unmarshal(emailResp, &emailResult); err == nil && emailResult.Notification != nil {
				emailResult.Notification.Name = "example-email"
				templates = append(templates, *emailResult.Notification)
			}
		}

		// Get slack template
		slackResp, err := c.request("GET", "/api/notifications/oddk:template:slack", nil)
		if err == nil {
			var slackResult operations.NotificationGetResult
			if err := json.Unmarshal(slackResp, &slackResult); err == nil && slackResult.Notification != nil {
				slackResult.Notification.Name = "example-slack"
				templates = append(templates, *slackResult.Notification)
			}
		}

		// Get webhook template
		webhookResp, err := c.request("GET", "/api/notifications/oddk:template:webhook", nil)
		if err == nil {
			var webhookResult operations.NotificationGetResult
			if err := json.Unmarshal(webhookResp, &webhookResult); err == nil && webhookResult.Notification != nil {
				webhookResult.Notification.Name = "example-webhook"
				templates = append(templates, *webhookResult.Notification)
			}
		}

		// If we couldn't get templates from server, return empty array
		if len(templates) == 0 {
			templates = []notifications.Notification{}
		}

		output, err := json.MarshalIndent(templates, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal template: %w", err)
		}

		_, _ = fmt.Fprintf(c.out, "%s\n", output)
		return nil
	}

	// Return the actual configurations as an array (strip system fields for cleaner output)
	var cleanNotifications []map[string]any
	for _, n := range result.Notifications {
		cleanNotification := map[string]any{
			"name":   n.Name,
			"type":   n.Type,
			"config": n.Config,
		}
		cleanNotifications = append(cleanNotifications, cleanNotification)
	}

	output, err := json.MarshalIndent(cleanNotifications, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal notifications: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", output)
	return nil
}

func (c *Client) notifyApplyAction(ctx context.Context, cmd *cli.Command) error {
	filePath := cmd.String("file")
	if filePath == "" {
		return fmt.Errorf("--file flag is required")
	}

	// Read the configuration file
	data, err := os.ReadFile(filePath) //nolint:gosec // User-specified file from CLI arg
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	// Parse as array of notifications
	var newNotifications []notifications.Notification
	if err := json.Unmarshal(data, &newNotifications); err != nil {
		return fmt.Errorf("invalid JSON in config file: %w", err)
	}

	// Check for duplicate names within the file
	nameMap := make(map[string]bool)
	for _, notif := range newNotifications {
		if nameMap[notif.Name] {
			return fmt.Errorf("duplicate notification name in file: %s", notif.Name)
		}
		nameMap[notif.Name] = true
	}

	// First, get existing notifications
	resp, err := c.request("GET", "/api/notifications", nil)
	if err != nil {
		return err
	}

	var existingResult operations.NotificationListResult
	if err := json.Unmarshal(resp, &existingResult); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	// Build a map of existing notifications by name
	existingMap := make(map[string]bool)
	for _, n := range existingResult.Notifications {
		existingMap[n.Name] = true
	}

	// Remove all existing notifications first
	for _, n := range existingResult.Notifications {
		_, err := c.request("DELETE", fmt.Sprintf("/api/notifications/%s", n.Name), nil)
		if err != nil {
			return fmt.Errorf("failed to remove existing notification %s: %w", n.Name, err)
		}
	}

	// Add all new notifications
	var added []string
	var updated []string

	for _, notification := range newNotifications {
		req := map[string]any{
			"name":   notification.Name,
			"type":   notification.Type,
			"config": notification.Config,
		}

		_, err := c.request("POST", "/api/notifications", req)
		if err != nil {
			return fmt.Errorf("failed to add notification %s: %w", notification.Name, err)
		}

		if existingMap[notification.Name] {
			updated = append(updated, notification.Name)
		} else {
			added = append(added, notification.Name)
		}
	}

	_, _ = fmt.Fprintf(c.out, "Notification configuration applied successfully\n")
	if len(added) > 0 {
		_, _ = fmt.Fprintf(c.out, "Added: %s\n", strings.Join(added, ", "))
	}
	if len(updated) > 0 {
		_, _ = fmt.Fprintf(c.out, "Updated: %s\n", strings.Join(updated, ", "))
	}

	return nil
}

func (c *Client) notifyHelpAddAction(ctx context.Context, cmd *cli.Command) error {
	notifType := cmd.String("type")

	// Early validation for clearer error messages
	if err := notifications.ValidateNotificationType(notifType); err != nil {
		return fmt.Errorf("invalid notification type '%s': supported types are email, slack, telegram, webhook", notifType)
	}

	// Request template from server
	resp, err := c.request("GET", fmt.Sprintf("/api/notifications/oddk:template:%s", notifType), nil)
	if err != nil {
		return err
	}

	var response operations.NotificationGetResult
	if err := json.Unmarshal(resp, &response); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if response.Notification == nil {
		return fmt.Errorf("no notification in response")
	}

	notification := *response.Notification

	// Update the name and ensure type is set
	notification.Name = fmt.Sprintf("example-%s", notifType)
	notification.Type = notifications.NotificationType(notifType)

	// Create a clean template structure without system fields
	template := struct {
		Name   string                         `json:"name"`
		Type   notifications.NotificationType `json:"type"`
		Config json.RawMessage                `json:"config"`
	}{
		Name:   notification.Name,
		Type:   notification.Type,
		Config: notification.Config,
	}

	// Output to stdout
	data, err := json.MarshalIndent(template, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal template: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", data)
	return nil
}

func (c *Client) notifyTestAction(ctx context.Context, cmd *cli.Command) error {
	resp, err := c.request("POST", "/api/notifications/test", nil)
	if err != nil {
		return err
	}

	var result struct {
		Message string `json:"message"`
		Results []struct {
			Name    string `json:"name"`
			Success bool   `json:"success"`
			Error   string `json:"error,omitempty"`
		} `json:"results"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n\n", result.Message)

	if len(result.Results) > 0 {
		for _, r := range result.Results {
			if r.Success {
				_, _ = fmt.Fprintf(c.out, "✓ %s: Success\n", r.Name)
			} else {
				_, _ = fmt.Fprintf(c.out, "✗ %s: Failed - %s\n", r.Name, r.Error)
			}
		}
	}

	return nil
}

func (c *Client) notifyRemoveAction(ctx context.Context, cmd *cli.Command) error {
	force := cmd.Bool("force")

	if !force {
		confirmed, err := c.cliConfirm("Are you sure you want to remove all notification configurations? [y/N]: ")
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(c.out, "Removal cancelled")
			return nil
		}
	}

	// Get all notifications first
	resp, err := c.request("GET", "/api/notifications", nil)
	if err != nil {
		return err
	}

	var result operations.NotificationListResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if len(result.Notifications) == 0 {
		_, _ = fmt.Fprintln(c.out, "No notifications to remove")
		return nil
	}

	// Remove all notifications
	for _, n := range result.Notifications {
		_, err := c.request("DELETE", fmt.Sprintf("/api/notifications/%s", n.Name), nil)
		if err != nil {
			return fmt.Errorf("failed to remove notification %s: %w", n.Name, err)
		}
	}

	_, _ = fmt.Fprintf(c.out, "All %d notification(s) removed successfully\n", len(result.Notifications))
	return nil
}

func (c *Client) notifyLogsAction(ctx context.Context, cmd *cli.Command) error {
	limit := cmd.Int("limit")

	resp, err := c.request("GET", fmt.Sprintf("/api/notifications/logs?limit=%d", limit), nil)
	if err != nil {
		return err
	}

	var result operations.NotificationLogsResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	logs := result.Logs

	if len(logs) == 0 {
		_, _ = fmt.Fprintln(c.out, "No notification logs found")
		return nil
	}

	headers := []string{"ID", "NAME", "STATUS", "TIME", "MESSAGE/ERROR"}
	var rows [][]string

	for _, log := range logs {
		msgOrErr := ""
		if log.Message != nil {
			msgOrErr = *log.Message
		}
		if log.Error != nil && *log.Error != "" {
			msgOrErr = *log.Error
		}
		// Truncate long messages
		if len(msgOrErr) > 50 {
			msgOrErr = msgOrErr[:47] + "..."
		}

		rows = append(rows, []string{
			fmt.Sprintf("%d", log.ID),
			log.NotificationName,
			log.Status,
			log.CreatedAt.Format("2006-01-02 15:04:05"),
			msgOrErr,
		})
	}

	return writeTable(c.out, headers, rows)
}
