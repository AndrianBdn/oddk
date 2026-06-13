package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli/v3"
)

func (c *Client) parametersGetAction(ctx context.Context, cmd *cli.Command) error {
	name := cmd.String("name")
	outputJSON := cmd.Bool("json")

	if name != "" {
		// Get specific parameter group
		path := fmt.Sprintf("/api/parameters/%s", name)
		body, err := c.request("GET", path, nil)
		if err != nil {
			return err
		}

		if outputJSON {
			_, _ = fmt.Fprintln(c.out, string(body))
			return nil
		}

		var result struct {
			GroupName  string `json:"groupName"`
			Parameters []struct {
				Name      string `json:"name"`
				Type      string `json:"type"`
				ValueType string `json:"valueType"`
				Value     string `json:"value"`
			} `json:"parameters"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		_, _ = fmt.Fprintf(c.out, "Parameter Group: %s\n\n", result.GroupName)

		headers := []string{"NAME", "TYPE", "VALUE_TYPE", "VALUE"}
		var rows [][]string
		for _, param := range result.Parameters {
			rows = append(rows, []string{
				param.Name,
				param.Type,
				param.ValueType,
				param.Value,
			})
		}

		return writeTable(c.out, headers, rows)
	} else {
		// List all parameter groups
		path := "/api/parameters"
		body, err := c.request("GET", path, nil)
		if err != nil {
			return err
		}

		if outputJSON {
			_, _ = fmt.Fprintln(c.out, string(body))
			return nil
		}

		var result struct {
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
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		if len(result.Groups) == 0 {
			_, _ = fmt.Fprintln(c.out, "No parameter groups found")
			return nil
		}

		// Show summary of parameter groups with counts by type
		headers := []string{"GROUP NAME", "PARAMETERS", "TYPES"}
		var rows [][]string

		for _, group := range result.Groups {
			typeCount := make(map[string]int)
			for _, param := range group.Parameters {
				typeCount[param.Type]++
			}

			var typeStrs []string
			for paramType, count := range typeCount {
				typeStrs = append(typeStrs, fmt.Sprintf("%s: %d", paramType, count))
			}

			rows = append(rows, []string{
				group.Name,
				fmt.Sprintf("%d", len(group.Parameters)),
				strings.Join(typeStrs, ", "),
			})
		}

		return writeTable(c.out, headers, rows)
	}
}

func (c *Client) parametersPutAction(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) != 1 {
		return fmt.Errorf("group name is required")
	}

	groupName := args[0]
	filePath := cmd.String("file")

	// Read the JSON file
	fileData, err := os.ReadFile(filePath) //nolint:gosec // User-specified file path
	if err != nil {
		return fmt.Errorf("read parameters file: %w", err)
	}

	// Validate JSON
	var parameters []map[string]any
	if err := json.Unmarshal(fileData, &parameters); err != nil {
		return fmt.Errorf("invalid JSON in parameters file: %w", err)
	}

	// Create request payload
	payload := map[string]any{
		"parameters": json.RawMessage(fileData),
	}

	// Make API request
	path := fmt.Sprintf("/api/parameters/%s", groupName)
	body, err := c.request("PUT", path, payload)
	if err != nil {
		return err
	}

	var result struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintln(c.out, result.Message)
	return nil
}

func (c *Client) parametersDeleteAction(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) != 1 {
		return fmt.Errorf("usage: parameters delete <group-name>")
	}

	groupName := args[0]
	force := cmd.Bool("force")

	if !force {
		confirmed, err := c.cliConfirm(fmt.Sprintf("Are you sure you want to delete parameter group '%s'? (y/N): ", groupName))
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(c.out, "Cancelled.")
			return nil
		}
	}

	path := fmt.Sprintf("/api/parameters/%s", groupName)
	body, err := c.request("DELETE", path, nil)
	if err != nil {
		return err
	}

	var result struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintln(c.out, result.Message)
	return nil
}
