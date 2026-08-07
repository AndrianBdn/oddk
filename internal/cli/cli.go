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
	// Some invocations never reach the daemon's HTTP API and so need no client
	// or auth token. Build the full app with an empty client and skip NewClient
	// entirely for them, so its "no token found" nudge never fires:
	//   - daemon / auth subcommands run locally, talking to the database/host
	//     directly. This matters for `auth mint` in particular: its stdout is
	//     eval'd by the caller (eval "$(sudo -u oddk oddk auth mint)"), so the
	//     nudge must never reach stdout and corrupt the emitted shell.
	//   - --version/--help (and a bare `oddk`) are purely informational; a
	//     missing token is irrelevant to them and the nudge is just noise.
	if localOnlyInvocation(args) {
		// envMap must still be populated: local-only commands with confirmation
		// prompts (snapshot apply) honour ODDK_SKIP_CONFIRM through it.
		app := BuildApp(&Client{out: out, envMap: parseEnv(env)})
		app.Writer = out
		return app.Run(context.Background(), args)
	}

	// For all other commands, initialize client
	client, err := NewClient(env, out)
	if err != nil {
		// If it's just missing auth token, don't fail but print help
		if strings.Contains(err.Error(), "auth token not configured") {
			// Continue with nil client for help display
			client = &Client{out: out, envMap: parseEnv(env)}
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

// localOnlyInvocation reports whether the given argv targets a command that
// runs without contacting the daemon, so NewClient (and its missing-token
// nudge) can be skipped entirely. This covers the local subcommands plus the
// top-level version/help flags and a bare `oddk` (which just prints help).
func localOnlyInvocation(args []string) bool {
	if len(args) <= 1 {
		// `oddk` with no arguments prints help.
		return true
	}
	switch args[1] {
	case "daemon", "auth",
		"--version", "-v",
		"--help", "-h", "help":
		return true
	case "snapshot":
		if len(args) <= 2 {
			return false
		}
		// `snapshot apply` runs daemon-less by design — disaster recovery must
		// work when the daemon cannot start — so the missing-token nudge is
		// pure noise on the one host guaranteed not to have a token yet: a
		// freshly installed DR host.
		if args[2] == "apply" {
			return true
		}
		// `snapshot list-remote s3://...` is equally daemon-less (only its
		// zero-arg form talks to the daemon). Going through NewClient here
		// would nudge on a tokenless DR host — and an invalid stale CLI config
		// would fail outright the one command that host runs FIRST. An s3://
		// argument is unambiguous, so sniffing argv for it is safe.
		if args[2] == "list-remote" {
			for _, arg := range args[3:] {
				if strings.HasPrefix(arg, "s3://") {
					return true
				}
			}
		}
		return false
	}
	return false
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
		// machine-consumed (e.g. `auth mint` output is eval'd by the caller).
		_, _ = fmt.Fprintln(os.Stderr, "No auth token found. Mint one as the oddk service user:")
		_, _ = fmt.Fprintln(os.Stderr, "  eval \"$(sudo -u oddk oddk auth mint)\"")
		_, _ = fmt.Fprintln(os.Stderr, "\nThis installs ~/.config/oddk/cli.json. The CLI also reads")
		_, _ = fmt.Fprintln(os.Stderr, ".oddk-cli.json in the current directory. Config format:")
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
	// Run() builds a config-less client when no auth token is found so that
	// --help still works; any command that actually reaches the daemon must
	// fail cleanly here instead of dereferencing a nil config.
	if c.config == nil {
		return nil, fmt.Errorf("auth token not configured (see ~/.config/oddk/cli.json)")
	}

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
