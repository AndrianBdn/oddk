package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/hypersequent/oddk/internal/store/offsite"
)

func (c *Client) offsiteInfoAction(ctx context.Context, cmd *cli.Command) error {
	resp, err := c.request("GET", "/api/offsite", nil)
	if err != nil {
		return fmt.Errorf("failed to get offsite info: %w", err)
	}

	var result struct {
		Active bool                     `json:"active"`
		Config *offsite.OffsiteSettings `json:"config,omitempty"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if !result.Active {
		_, _ = fmt.Fprintf(c.out, "No offsite configuration is active\n")
		return nil
	}

	_, _ = fmt.Fprintf(c.out, "Offsite Configuration:\n")
	_, _ = fmt.Fprintf(c.out, "  Type:         %s\n", result.Config.Type)
	_, _ = fmt.Fprintf(c.out, "  Bucket:       %s\n", result.Config.Bucket)
	if result.Config.Endpoint != nil {
		_, _ = fmt.Fprintf(c.out, "  Endpoint:     %s\n", *result.Config.Endpoint)
	}
	if result.Config.Region != nil {
		_, _ = fmt.Fprintf(c.out, "  Region:       %s\n", *result.Config.Region)
	}
	_, _ = fmt.Fprintf(c.out, "  Access Key:   %s\n", result.Config.AccessKeyID)
	if result.Config.BucketPath != nil {
		_, _ = fmt.Fprintf(c.out, "  Bucket Path:  %s\n", *result.Config.BucketPath)
	}
	_, _ = fmt.Fprintf(c.out, "  Created:      %s\n", result.Config.CreatedAt.Format("2006-01-02 15:04:05"))
	_, _ = fmt.Fprintf(c.out, "  Updated:      %s\n", result.Config.UpdatedAt.Format("2006-01-02 15:04:05"))

	return nil
}

func (c *Client) offsiteLogsAction(ctx context.Context, cmd *cli.Command) error {
	limit := cmd.Int("limit")

	resp, err := c.request("GET", fmt.Sprintf("/api/offsite/logs?limit=%d", limit), nil)
	if err != nil {
		return fmt.Errorf("failed to get offsite logs: %w", err)
	}

	var logs []offsite.OffsiteLog
	if err := json.Unmarshal(resp, &logs); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if len(logs) == 0 {
		_, _ = fmt.Fprintf(c.out, "No offsite logs found\n")
		return nil
	}

	_, _ = fmt.Fprintf(c.out, "Showing %d offsite log entries:\n\n", len(logs))
	for _, log := range logs {
		status := "✓ Success"
		if !log.Success {
			status = "✗ Failed"
		}
		_, _ = fmt.Fprintf(c.out, "[%s] %s - %s - %s\n",
			log.CreatedAt.Format("2006-01-02 15:04:05"),
			status,
			log.Event,
			log.Object)
		if log.ErrorDetails != nil && *log.ErrorDetails != "" {
			_, _ = fmt.Fprintf(c.out, "  Error: %s\n", *log.ErrorDetails)
		}
	}

	return nil
}

func (c *Client) offsiteGetAction(ctx context.Context, cmd *cli.Command) error {
	resp, err := c.request("GET", "/api/offsite/config", nil)
	if err != nil {
		return fmt.Errorf("failed to get offsite config: %w", err)
	}

	var result struct {
		Config offsite.OffsiteSettingsJSON `json:"config"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	// Pretty print the JSON
	output, err := json.MarshalIndent(result.Config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", string(output))
	return nil
}

func (c *Client) offsiteApplyAction(ctx context.Context, cmd *cli.Command) error {
	filePath := cmd.String("file")
	if filePath == "" {
		return fmt.Errorf("--file flag is required")
	}

	// Read the configuration file
	configData, err := os.ReadFile(filePath) //nolint:gosec // User-specified file from CLI arg
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	// Validate it's valid JSON
	var config offsite.OffsiteSettingsJSON
	if err := json.Unmarshal(configData, &config); err != nil {
		return fmt.Errorf("invalid JSON in config file: %w", err)
	}

	// Send to server
	_, err = c.request("PUT", "/api/offsite/config", config)
	if err != nil {
		return fmt.Errorf("failed to apply offsite config: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "Offsite configuration applied successfully\n")
	return nil
}

func (c *Client) offsiteRemoveAction(ctx context.Context, cmd *cli.Command) error {
	// Check if force flag is set
	if !cmd.Bool("force") {
		confirmed, err := c.cliConfirm("Are you sure you want to remove the offsite configuration? (y/N): ")
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintf(c.out, "Removal cancelled\n")
			return nil
		}
	}

	_, err := c.request("DELETE", "/api/offsite/config", nil)
	if err != nil {
		return fmt.Errorf("failed to remove offsite config: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "Offsite configuration removed successfully\n")
	return nil
}

func (c *Client) offsiteTestAction(ctx context.Context, cmd *cli.Command) error {
	_, _ = fmt.Fprintf(c.out, "Testing offsite configuration...\n")

	resp, err := c.request("POST", "/api/offsite/test", nil)
	if err != nil {
		return fmt.Errorf("failed to test offsite config: %w", err)
	}

	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Error   string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if result.Success {
		_, _ = fmt.Fprintf(c.out, "✓ Test passed: %s\n", result.Message)
	} else {
		_, _ = fmt.Fprintf(c.out, "✗ Test failed: %s\n", result.Error)
		return fmt.Errorf("offsite test failed")
	}

	return nil
}
