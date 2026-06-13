package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
)

type KVPair struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updatedAt"`
}

func (c *Client) customKVListAction(ctx context.Context, cmd *cli.Command) error {
	body, err := c.request("GET", "/api/customkv", nil)
	if err != nil {
		return err
	}

	var pairs []KVPair
	if err := json.Unmarshal(body, &pairs); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if len(pairs) == 0 {
		_, _ = fmt.Fprintln(c.out, "No custom key-value pairs found.")
		return nil
	}

	headers := []string{"KEY", "VALUE", "UPDATED"}
	var rows [][]string
	for _, pair := range pairs {
		// Parse and format timestamp
		updatedAt := pair.UpdatedAt
		if t, err := time.Parse("2006-01-02T15:04:05Z07:00", pair.UpdatedAt); err == nil {
			updatedAt = t.Format("2006-01-02 15:04:05")
		}

		rows = append(rows, []string{
			pair.Key,
			pair.Value,
			updatedAt,
		})
	}

	return writeTable(c.out, headers, rows)
}

func (c *Client) customKVGetAction(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) == 0 {
		return fmt.Errorf("key argument is required")
	}

	key := args[0]
	path := fmt.Sprintf("/api/customkv/%s", key)

	body, err := c.request("GET", path, nil)
	if err != nil {
		return err
	}

	var pair KVPair
	if err := json.Unmarshal(body, &pair); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	// Parse and format timestamp
	updatedAt := pair.UpdatedAt
	if t, err := time.Parse("2006-01-02T15:04:05Z07:00", pair.UpdatedAt); err == nil {
		updatedAt = t.Format("2006-01-02 15:04:05")
	}

	_, _ = fmt.Fprintf(c.out, "Key:     %s\n", pair.Key)
	_, _ = fmt.Fprintf(c.out, "Value:   %s\n", pair.Value)
	_, _ = fmt.Fprintf(c.out, "Updated: %s\n", updatedAt)

	return nil
}

func (c *Client) customKVSetAction(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("key argument is required")
	}

	key := args[0]
	value := cmd.String("value")

	// Validate key format
	if !strings.HasSuffix(key, ".str") && !strings.HasSuffix(key, ".int") {
		return fmt.Errorf("key must end with .str (for strings) or .int (for integers)")
	}

	// For integer keys, validate the value
	if strings.HasSuffix(key, ".int") {
		if !isValidInteger(value) {
			return fmt.Errorf("value must be a valid integer for .int keys")
		}
	}

	request := map[string]string{
		"value": value,
	}

	path := fmt.Sprintf("/api/customkv/%s", key)
	body, err := c.request("PUT", path, request)
	if err != nil {
		return err
	}

	var pair KVPair
	if err := json.Unmarshal(body, &pair); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "Set %s = %s\n", pair.Key, pair.Value)
	return nil
}

// Helper function to validate integer values
func isValidInteger(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if i == 0 && r == '-' {
			continue // Allow negative numbers
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
