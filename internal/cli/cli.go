package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	DaemonURL string `json:"daemonUrl"`
	AuthToken string `json:"authToken"`
}

type Client struct {
	config *Config
	http   *http.Client
	out    io.Writer
	envMap map[string]string // Environment variables for testing
}

func Run(args, env []string, out io.Writer) error {
	// daemon and cli-auth run locally - they talk to the database/host directly,
	// not the daemon's HTTP API, so they need no client or auth token. Build the
	// full app with an empty client and skip NewClient entirely. This matters for
	// cli-auth in particular: its stdout is eval'd by the caller
	// (eval "$(sudo -u oddk oddk cli-auth)"), so NewClient's "no token found"
	// message must never reach stdout and corrupt the emitted shell.
	if len(args) > 1 && (args[1] == "daemon" || args[1] == "cli-auth") {
		app := BuildApp(&Client{out: out})
		app.Writer = out
		return app.Run(context.Background(), args)
	}

	// For all other commands, initialize client
	client, err := NewClient(env, out)
	if err != nil {
		// If it's just missing auth token, don't fail but print help
		if strings.Contains(err.Error(), "auth token not configured") {
			// Continue with nil client for help display
			client = &Client{out: out}
		} else {
			return fmt.Errorf("failed to initialize client: %w", err)
		}
	}

	app := BuildApp(client)
	app.Writer = out
	return app.Run(context.Background(), args)
}

func Execute() error {
	return Run(os.Args, os.Environ(), os.Stdout)
}

func NewClient(env []string, out io.Writer) (*Client, error) {
	config := &Config{
		DaemonURL: "http://localhost:5442",
	}

	// Check for environment variables
	envMap := parseEnv(env)

	// Check if ODDK_CLI_CONFIG env var points to a specific config file
	if configPath, ok := envMap["ODDK_CLI_CONFIG"]; ok {
		if data, err := os.ReadFile(configPath); err == nil { //nolint:gosec // User-specified config file path from env
			if err := json.Unmarshal(data, config); err != nil {
				return nil, fmt.Errorf("invalid config file %s: %w", configPath, err)
			}
		} else {
			return nil, fmt.Errorf("could not read config file %s: %w", configPath, err)
		}
	} else {
		// Try to load config from current directory first
		cwdConfigPath := ".oddk-cli.json"
		if data, err := os.ReadFile(cwdConfigPath); err == nil {
			if err := json.Unmarshal(data, config); err != nil {
				return nil, fmt.Errorf("invalid config file %s: %w", cwdConfigPath, err)
			}
		} else {
			// Fall back to home directory config
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("could not determine home directory: %w", err)
			}

			homeConfigPath := filepath.Join(home, ".config", "oddk", "cli.json")
			if data, err := os.ReadFile(homeConfigPath); err == nil { //nolint:gosec // Path is constructed from safe components
				if err := json.Unmarshal(data, config); err != nil {
					return nil, fmt.Errorf("invalid config file %s: %w", homeConfigPath, err)
				}
			}
		}
	}

	if config.AuthToken == "" {
		// Diagnostics go to stderr, never stdout - some commands' stdout is
		// machine-consumed (e.g. cli-auth output is eval'd by the caller).
		_, _ = fmt.Fprintln(os.Stderr, "No auth token found. Please run the daemon first and save the token to:")
		_, _ = fmt.Fprintln(os.Stderr, "  - .oddk-cli.json (in current directory)")
		_, _ = fmt.Fprintln(os.Stderr, "  - ~/.config/oddk/cli.json (in home directory)")
		_, _ = fmt.Fprintln(os.Stderr, "\nExample config:")
		_, _ = fmt.Fprintln(os.Stderr, `{
  "daemonUrl": "http://localhost:5442",
  "authToken": "YOUR_TOKEN_HERE"
}`)
		return nil, fmt.Errorf("auth token not configured")
	}

	return &Client{
		config: config,
		http:   &http.Client{},
		out:    out,
		envMap: envMap,
	}, nil
}

func parseEnv(env []string) map[string]string {
	result := make(map[string]string)
	for _, e := range env {
		if idx := strings.Index(e, "="); idx > 0 {
			result[e[:idx]] = e[idx+1:]
		}
	}
	return result
}

func (c *Client) request(method, path string, body any) ([]byte, error) {
	url := c.config.DaemonURL + path

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(data)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.config.AuthToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close() // Ignore error in defer
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			return nil, fmt.Errorf("%s", errResp.Error)
		}
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, respBody)
	}

	return respBody, nil
}

// cliConfirm prompts the user for confirmation and returns true if they confirm.
// It respects the ODDK_SKIP_CONFIRM environment variable to skip prompts.
// If the output is a buffer, it warns that confirmation prompts should be skipped.
func (c *Client) cliConfirm(message string) (bool, error) {
	// Check if confirmation should be skipped via environment variable
	if _, ok := c.envMap["ODDK_SKIP_CONFIRM"]; ok {
		return true, nil
	}

	// Warn if output is a buffer (likely in tests)
	if _, isBuffer := c.out.(*bytes.Buffer); isBuffer {
		_, _ = fmt.Fprintf(c.out, "WARNING: Confirmation prompt detected with buffer output. Set ODDK_SKIP_CONFIRM=1 to skip prompts.\n")
	}

	// Display the confirmation message
	_, _ = fmt.Fprint(c.out, message)

	// Read user input
	var response string
	if _, err := fmt.Scanln(&response); err != nil {
		return false, fmt.Errorf("read user input: %w", err)
	}

	// Check if user confirmed (y or Y)
	return response == "y" || response == "Y", nil
}
