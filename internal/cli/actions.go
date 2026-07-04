package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/jmoiron/sqlx"
	"github.com/moby/term"
	"github.com/urfave/cli/v3"

	"github.com/hypersequent/oddk/internal/daemon"
	"github.com/hypersequent/oddk/internal/store"
	"github.com/hypersequent/oddk/internal/store/auth"
	"github.com/hypersequent/oddk/internal/store/parameters"
	"github.com/hypersequent/oddk/internal/util"
)

// Pull action
func (c *Client) pullAction(ctx context.Context, cmd *cli.Command) error {
	version := cmd.String("version")
	image := cmd.String("image")

	if version == "" && image == "" {
		return fmt.Errorf("at least one of --version or --image is required")
	}

	req := map[string]any{
		"version": version,
		"image":   image,
	}

	return c.streamProgress(ctx, "POST", "/api/pull", req)
}

// streamProgress sends an authenticated request whose response is a stream of
// newline-delimited Docker progress frames (jsonmessage) and renders them to
// c.out — live progress bars on a terminal, plain status lines otherwise. body
// may be nil for a request without a payload. It returns a non-nil error if the
// operation failed (the error is carried inside the stream as an error frame,
// or returned as a non-streaming 4xx/5xx before streaming begins).
func (c *Client) streamProgress(ctx context.Context, method, path string, body any) error {
	var payload io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.config.DaemonURL+path, payload)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.config.AuthToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			return fmt.Errorf("%s", errResp.Error)
		}
		return fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	fd, isTerm := term.GetFdInfo(c.out)
	return jsonmessage.DisplayJSONMessagesStream(resp.Body, c.out, fd, isTerm, nil)
}

// ensureImage makes sure the image an operation needs is present locally,
// pulling it with streamed progress only when missing (a cached image is reused
// without a network round-trip). version/image mirror the command's flags; the
// daemon resolves the concrete image name.
func (c *Client) ensureImage(ctx context.Context, version, image string) error {
	return c.streamProgress(ctx, "POST", "/api/pull", map[string]any{
		"version":   version,
		"image":     image,
		"ifMissing": true,
	})
}

// requireInstanceName is a helper that shows help and returns an error if no instance name is provided
func requireInstanceName(cmd *cli.Command) (string, error) {
	if cmd.Args().Len() < 1 {
		_ = cli.ShowSubcommandHelp(cmd)
		return "", fmt.Errorf("instance name required")
	}
	return cmd.Args().Get(0), nil
}

// Create action
func (c *Client) createAction(ctx context.Context, cmd *cli.Command) error {
	name := cmd.String("name")
	version := cmd.String("version")
	image := cmd.String("image")
	port := cmd.Int("port")
	cpuCores := cmd.Int("cpu")
	ramStr := cmd.String("ram")
	parameterGroup := cmd.String("parameter-group")

	// Validate required flags
	if cpuCores == 0 {
		return fmt.Errorf("--cpu flag is required")
	}
	if ramStr == "" {
		return fmt.Errorf("--ram flag is required")
	}

	// Parse RAM string
	ramMB, err := util.ParseRAMString(ramStr)
	if err != nil {
		return fmt.Errorf("invalid RAM value: %w", err)
	}

	// Basic client-side validation with reasonable bounds
	if err := util.ValidateBasicResourceBounds(cpuCores, ramMB); err != nil {
		return fmt.Errorf("invalid resource values: %w", err)
	}

	// Use default parameter group if not specified
	if parameterGroup == "" {
		parameterGroup = parameters.DefaultParameterGroup
	}

	// Auto-provision the image (pull only if missing, with live progress) so
	// create works without a separate 'oddk pull' first.
	if err := c.ensureImage(ctx, version, image); err != nil {
		return err
	}

	req := map[string]any{
		"name":           name,
		"version":        version,
		"image":          image,
		"port":           port,
		"cpuCores":       cpuCores,
		"ramMB":          ramMB,
		"parameterGroup": parameterGroup,
	}

	resp, err := c.request("POST", "/api/rdbms", req)
	if err != nil {
		return err
	}

	var instance struct {
		Name     string `json:"name"`
		Port     int    `json:"port"`
		Version  string `json:"version"`
		Status   string `json:"status"`
		Password string `json:"password"`
		CPUCores int    `json:"cpuCores"`
		RAMMB    int    `json:"ramMb"`
	}

	if err := json.Unmarshal(resp, &instance); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "Created RDBMS instance: %s\n", instance.Name)
	_, _ = fmt.Fprintf(c.out, "PostgreSQL version: %s\n", instance.Version)
	_, _ = fmt.Fprintf(c.out, "Port: %d\n", instance.Port)
	_, _ = fmt.Fprintf(c.out, "CPU Cores: %d\n", instance.CPUCores)
	_, _ = fmt.Fprintf(c.out, "RAM: %d MB\n", instance.RAMMB)
	_, _ = fmt.Fprintf(c.out, "Status: %s\n", instance.Status)
	_, _ = fmt.Fprintf(c.out, "Password: %s\n", instance.Password)
	_, _ = fmt.Fprintf(c.out, "\nConnection string:\n")
	_, _ = fmt.Fprintf(c.out, "postgresql://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable\n", instance.Password, instance.Port)

	return nil
}

// List action
func (c *Client) listAction(ctx context.Context, cmd *cli.Command) error {
	resp, err := c.request("GET", "/api/rdbms", nil)
	if err != nil {
		return err
	}

	var instances []struct {
		Name           string `json:"name"`
		Port           int    `json:"port"`
		Version        string `json:"version"`
		Status         string `json:"status"`
		CPUCores       int    `json:"cpuCores"`
		RAMMB          int    `json:"ramMb"`
		ParameterGroup string `json:"parameterGroup"`
		CreatedAt      string `json:"createdAt"`
	}

	if err := json.Unmarshal(resp, &instances); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if len(instances) == 0 {
		_, _ = fmt.Fprintln(c.out, "No RDBMS instances found")
		return nil
	}

	headers := []string{"NAME", "VERSION", "PORT", "CPU", "RAM", "PARAMETER GROUP", "STATUS", "CREATED"}
	var rows [][]string

	for _, inst := range instances {
		created := inst.CreatedAt
		if t := strings.Index(created, "T"); t > 0 {
			created = created[:t]
		}
		rows = append(rows, []string{
			inst.Name,
			inst.Version,
			fmt.Sprintf("%d", inst.Port),
			fmt.Sprintf("%d", inst.CPUCores),
			fmt.Sprintf("%dMB", inst.RAMMB),
			inst.ParameterGroup,
			inst.Status,
			created,
		})
	}

	return writeTable(c.out, headers, rows)
}

// Instance actions
func (c *Client) statusAction(ctx context.Context, cmd *cli.Command) error {
	name, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	resp, err := c.request("GET", fmt.Sprintf("/api/rdbms/%s", name), nil)
	if err != nil {
		return err
	}

	var instance struct {
		Name      string `json:"name"`
		Port      int    `json:"port"`
		Version   string `json:"version"`
		Status    string `json:"status"`
		CPUCores  int    `json:"cpuCores"`
		RAMMB     int    `json:"ramMb"`
		CreatedAt string `json:"createdAt"`
	}

	if err := json.Unmarshal(resp, &instance); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "Instance: %s\n", instance.Name)
	_, _ = fmt.Fprintf(c.out, "Status: %s\n", instance.Status)
	_, _ = fmt.Fprintf(c.out, "Version: PostgreSQL %s\n", instance.Version)
	_, _ = fmt.Fprintf(c.out, "Port: %d\n", instance.Port)
	_, _ = fmt.Fprintf(c.out, "CPU Cores: %d\n", instance.CPUCores)
	_, _ = fmt.Fprintf(c.out, "RAM: %d MB\n", instance.RAMMB)
	_, _ = fmt.Fprintf(c.out, "Created: %s\n", instance.CreatedAt)

	return nil
}

func (c *Client) startAction(ctx context.Context, cmd *cli.Command) error {
	name, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	req := map[string]string{"state": "start"}
	_, err = c.request("PUT", fmt.Sprintf("/api/rdbms/%s/state", name), req)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(c.out, "Started instance: %s\n", name)
	return nil
}

func (c *Client) stopAction(ctx context.Context, cmd *cli.Command) error {
	name, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	req := map[string]string{"state": "stop"}
	_, err = c.request("PUT", fmt.Sprintf("/api/rdbms/%s/state", name), req)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(c.out, "Stopped instance: %s\n", name)
	return nil
}

func (c *Client) destroyAction(ctx context.Context, cmd *cli.Command) error {
	name, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}
	force := cmd.Bool("force")

	if !force {
		confirmed, err := c.cliConfirm(fmt.Sprintf("Are you sure you want to destroy instance '%s'? This will delete all data. [y/N]: ", name))
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(c.out, "Cancelled")
			return nil
		}
	}

	_, err = c.request("DELETE", fmt.Sprintf("/api/rdbms/%s", name), nil)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(c.out, "Destroyed instance: %s\n", name)
	return nil
}

func (c *Client) listDatabasesAction(ctx context.Context, cmd *cli.Command) error {
	name, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	resp, err := c.request("GET", fmt.Sprintf("/api/rdbms/%s/databases", name), nil)
	if err != nil {
		return err
	}

	var result struct {
		Databases []struct {
			Name     string `json:"name"`
			Owner    string `json:"owner"`
			Encoding string `json:"encoding"`
			Size     string `json:"size"`
		} `json:"databases"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if len(result.Databases) == 0 {
		_, _ = fmt.Fprintf(c.out, "No databases found in instance %s\n", name)
		return nil
	}

	headers := []string{"NAME", "OWNER", "ENCODING", "SIZE"}
	var rows [][]string

	for _, db := range result.Databases {
		rows = append(rows, []string{
			db.Name,
			db.Owner,
			db.Encoding,
			db.Size,
		})
	}

	return writeTable(c.out, headers, rows)
}

func (c *Client) createDatabaseAction(ctx context.Context, cmd *cli.Command) error {
	instanceName, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}
	databaseName := cmd.String("database")
	username := cmd.String("username")

	req := map[string]any{
		"databaseName": databaseName,
		"username":     username,
	}

	resp, err := c.request("POST", fmt.Sprintf("/api/rdbms/%s/databases", instanceName), req)
	if err != nil {
		return err
	}

	var result struct {
		DatabaseName string `json:"databaseName"`
		Username     string `json:"username"`
		Password     string `json:"password"`
		Message      string `json:"message"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	// An older daemon ignores the unknown username field and creates just the
	// database — surface that instead of printing success without credentials.
	if username != "" && result.Username == "" {
		return fmt.Errorf("database %s was created, but the daemon ignored --username (it likely predates this option — upgrade the daemon, then add the user with add-db-user)", result.DatabaseName)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
	if result.Username != "" {
		c.printDBUserCredentials(instanceName, result.Username, result.Password, result.DatabaseName, "owner")
	}
	return nil
}

func (c *Client) getPasswordAction(ctx context.Context, cmd *cli.Command) error {
	name, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	plain := cmd.Bool("plain")
	conn := cmd.Bool("conn")
	envs := cmd.Bool("envs")

	// Get password from server
	resp, err := c.request("GET", fmt.Sprintf("/api/rdbms/%s/password", name), nil)
	if err != nil {
		return err
	}

	var result struct {
		Password string `json:"password"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	// Get instance details for formatting
	instResp, err := c.request("GET", fmt.Sprintf("/api/rdbms/%s", name), nil)
	if err != nil {
		return err
	}

	var instance struct {
		Name    string `json:"name"`
		Port    int    `json:"port"`
		Version string `json:"version"`
		Status  string `json:"status"`
	}

	if err := json.Unmarshal(instResp, &instance); err != nil {
		return fmt.Errorf("parse instance response: %w", err)
	}

	// Format output based on flags
	switch {
	case plain:
		_, _ = fmt.Fprintln(c.out, result.Password)
	case conn:
		connStr := fmt.Sprintf("postgresql://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable",
			result.Password, instance.Port)
		_, _ = fmt.Fprintln(c.out, connStr)
	case envs:
		_, _ = fmt.Fprintf(c.out, "export PGHOST=10.88.0.1\n")
		_, _ = fmt.Fprintf(c.out, "export PGPORT=%d\n", instance.Port)
		_, _ = fmt.Fprintf(c.out, "export PGUSER=postgres\n")
		_, _ = fmt.Fprintf(c.out, "export PGPASSWORD=%s\n", result.Password)
		_, _ = fmt.Fprintf(c.out, "export PGDATABASE=postgres\n")
	default:
		// Default format with structured output
		_, _ = fmt.Fprintf(c.out, "Instance: %s\n", instance.Name)
		_, _ = fmt.Fprintf(c.out, "Host: 10.88.0.1\n")
		_, _ = fmt.Fprintf(c.out, "Port: %d\n", instance.Port)
		_, _ = fmt.Fprintf(c.out, "Username: postgres\n")
		_, _ = fmt.Fprintf(c.out, "Password: %s\n", result.Password)
		_, _ = fmt.Fprintf(c.out, "Connection String: postgresql://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable\n",
			result.Password, instance.Port)
	}

	return nil
}

func (c *Client) setPasswordAction(ctx context.Context, cmd *cli.Command) error {
	name, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	// Check if NEW_PGPASSWORD is set
	password, ok := c.envMap["NEW_PGPASSWORD"]
	if !ok || password == "" {
		return fmt.Errorf("NEW_PGPASSWORD environment variable must be set with the new password")
	}

	// Send the password to the server
	req := map[string]string{
		"password": password,
	}

	resp, err := c.request("PUT", fmt.Sprintf("/api/rdbms/%s/password", name), req)
	if err != nil {
		return err
	}

	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintln(c.out, result.Message)
	return nil
}

// ensureDockerReachable confirms the docker binary is present AND that the
// current user can actually talk to the Docker daemon, returning the resolved
// docker path. `docker version` contacts the daemon socket, so a non-zero exit
// distinguishes "no Docker access" (e.g. user not in the docker group) from a
// missing binary. The returned error is actionable; docker's own message is
// included for context.
func ensureDockerReachable(ctx context.Context) (string, error) {
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return "", fmt.Errorf("docker not found in PATH: %w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	//nolint:gosec // dockerPath comes from exec.LookPath("docker"), not user input
	out, err := exec.CommandContext(probeCtx, dockerPath, "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			detail = "\n  " + strings.ReplaceAll(detail, "\n", "\n  ")
		}
		return "", fmt.Errorf("cannot reach Docker as the current user (%w):%s\n\n"+
			"`oddk instance psql` needs Docker access. Either add your user to the docker group:\n"+
			"  sudo usermod -aG docker $USER   # then log out and back in\n"+
			"or re-run the command with sudo", err, detail)
	}
	return dockerPath, nil
}

func (c *Client) psqlAction(ctx context.Context, cmd *cli.Command) error {
	name, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	// Get remaining args for psql
	var psqlArgs []string
	if cmd.Args().Len() > 1 {
		for i := 1; i < cmd.Args().Len(); i++ {
			psqlArgs = append(psqlArgs, cmd.Args().Get(i))
		}
	}

	// `psql` is the only command that talks to Docker directly (an interactive
	// psql TTY can't be proxied through the daemon's HTTP API), so it's the only
	// one that needs Docker access for the invoking user. Verify reachability now
	// with a clear message - once we syscall.Exec below, docker's own raw
	// socket-permission error is all the user would see.
	dockerPath, err := ensureDockerReachable(ctx)
	if err != nil {
		return err
	}

	// Get password from server
	resp, err := c.request("GET", fmt.Sprintf("/api/rdbms/%s/password", name), nil)
	if err != nil {
		return err
	}

	var result struct {
		Password string `json:"password"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	// Get instance details
	instResp, err := c.request("GET", fmt.Sprintf("/api/rdbms/%s", name), nil)
	if err != nil {
		return err
	}

	var instance struct {
		Name    string `json:"name"`
		Port    int    `json:"port"`
		Version string `json:"version"`
		Status  string `json:"status"`
	}

	if err := json.Unmarshal(instResp, &instance); err != nil {
		return fmt.Errorf("parse instance response: %w", err)
	}

	// Check if instance is running
	if instance.Status != "running" {
		return fmt.Errorf("instance %s is not running (status: %s)", name, instance.Status)
	}

	// Construct Docker command (preallocated: 17 fixed args + user psql args)
	dockerArgs := make([]string, 0, 17+len(psqlArgs))
	dockerArgs = append(dockerArgs,
		"run",
		"--rm",
		"-it",
		"--network", "oddk-bridge",
		"-e", "PGPASSWORD",
		fmt.Sprintf("postgres:%s", instance.Version),
		"psql",
		"-h", "10.88.0.1",
		"-p", fmt.Sprintf("%d", instance.Port),
		"-U", "postgres",
		"-d", "postgres",
	)

	// Add any additional psql arguments passed by the user
	dockerArgs = append(dockerArgs, psqlArgs...)

	// Set password in environment and inherit current environment
	env := os.Environ()
	env = append(env, fmt.Sprintf("PGPASSWORD=%s", result.Password))

	// Replace current process with docker
	//nolint:gosec // dockerPath is validated by exec.LookPath, safe to use in syscall.Exec
	if err := syscall.Exec(dockerPath, append([]string{"docker"}, dockerArgs...), env); err != nil {
		return fmt.Errorf("failed to exec docker: %w", err)
	}

	// This line should never be reached
	return nil
}

func (c *Client) addDatabaseUserAction(ctx context.Context, cmd *cli.Command) error {
	instanceName, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	username := cmd.String("username")
	database := cmd.String("database")
	readonly := cmd.Bool("readonly")
	owner := cmd.Bool("owner")

	if readonly && owner {
		return fmt.Errorf("--readonly and --owner are mutually exclusive")
	}

	req := map[string]any{
		"username": username,
		"readOnly": readonly,
		"owner":    owner,
	}

	resp, err := c.request("POST", fmt.Sprintf("/api/rdbms/%s/databases/%s/users", instanceName, database), req)
	if err != nil {
		// The one-shot create-db hint makes the user the database OWNER, so
		// never suggest it when read-only access was requested.
		if !readonly && strings.Contains(err.Error(), "does not exist") {
			return fmt.Errorf("%w\nHint: to create the database together with its user in one step, use:\n  oddk instance create-db %s --database %s --username %s\n(note: this makes the user the database owner)",
				err, instanceName, database, username)
		}
		return err
	}

	var result struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Database string `json:"database"`
		ReadOnly bool   `json:"readOnly"`
		Message  string `json:"message"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	access := "read-write"
	switch {
	case result.ReadOnly:
		access = "read-only"
	case owner:
		access = "owner"
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
	c.printDBUserCredentials(instanceName, result.Username, result.Password, result.Database, access)

	return nil
}

// printDBUserCredentials prints the one-time credentials block shared by
// add-db-user and create-db --username: generated password, connection
// string, and the not-saved warning.
func (c *Client) printDBUserCredentials(instanceName, username, password, database, access string) {
	_, _ = fmt.Fprintf(c.out, "\nCredentials:\n")
	_, _ = fmt.Fprintf(c.out, "Username: %s\n", username)
	_, _ = fmt.Fprintf(c.out, "Password: %s\n", password)
	_, _ = fmt.Fprintf(c.out, "Database: %s\n", database)
	_, _ = fmt.Fprintf(c.out, "Access: %s\n", access)

	// Get instance details for connection string
	instResp, err := c.request("GET", fmt.Sprintf("/api/rdbms/%s", instanceName), nil)
	if err == nil {
		var instance struct {
			Port int `json:"port"`
		}
		if json.Unmarshal(instResp, &instance) == nil {
			_, _ = fmt.Fprintf(c.out, "\nConnection string:\n")
			_, _ = fmt.Fprintf(c.out, "postgresql://%s:%s@10.88.0.1:%d/%s?sslmode=disable\n",
				username, password, instance.Port, database)
		}
	}

	_, _ = fmt.Fprintf(c.out, "\n⚠️  NOTE: This password is not saved. Save it securely now.\n")
	_, _ = fmt.Fprintf(c.out, "To reset the password later, use: oddk instance reset-db-user-password %s --username %s\n",
		instanceName, username)
}

func (c *Client) deleteDatabaseUserAction(ctx context.Context, cmd *cli.Command) error {
	instanceName, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	username := cmd.String("username")
	force := cmd.Bool("force")

	if !force {
		_, _ = fmt.Fprintf(c.out, "⚠️  This will delete user %s and revoke all their permissions.\n", username)
		confirmed, err := c.cliConfirm("Are you sure? (y/N): ")
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintf(c.out, "Deletion cancelled.\n")
			return nil
		}
	}

	resp, err := c.request("DELETE", fmt.Sprintf("/api/rdbms/%s/users/%s", instanceName, username), nil)
	if err != nil {
		return err
	}

	var result struct {
		Username string `json:"username"`
		Message  string `json:"message"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
	return nil
}

func (c *Client) resetDatabaseUserPasswordAction(ctx context.Context, cmd *cli.Command) error {
	instanceName, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	username := cmd.String("username")

	resp, err := c.request("PUT", fmt.Sprintf("/api/rdbms/%s/users/%s/password", instanceName, username), nil)
	if err != nil {
		return err
	}

	var result struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Message  string `json:"message"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
	_, _ = fmt.Fprintf(c.out, "\nNew credentials:\n")
	_, _ = fmt.Fprintf(c.out, "Username: %s\n", result.Username)
	_, _ = fmt.Fprintf(c.out, "Password: %s\n", result.Password)

	// Get instance details for connection string
	instResp, err := c.request("GET", fmt.Sprintf("/api/rdbms/%s", instanceName), nil)
	if err == nil {
		var instance struct {
			Port int `json:"port"`
		}
		if json.Unmarshal(instResp, &instance) == nil {
			_, _ = fmt.Fprintf(c.out, "\nConnection string:\n")
			_, _ = fmt.Fprintf(c.out, "postgresql://%s:%s@10.88.0.1:%d/<database>?sslmode=disable\n",
				result.Username, result.Password, instance.Port)
		}
	}

	_, _ = fmt.Fprintf(c.out, "\n⚠️  NOTE: This password is not saved. Save it securely now.\n")

	return nil
}

func (c *Client) logsAction(ctx context.Context, cmd *cli.Command) error {
	name, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	tail := cmd.String("tail")
	follow := cmd.Bool("follow")

	if follow {
		return c.streamLogs(ctx, name, tail)
	}

	resp, err := c.request("GET", fmt.Sprintf("/api/rdbms/%s/logs?tail=%s", name, tail), nil)
	if err != nil {
		return err
	}

	var result struct {
		Logs string `json:"logs"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprint(c.out, result.Logs)
	return nil
}

func (c *Client) streamLogs(ctx context.Context, name, tail string) error {
	url := fmt.Sprintf("%s/api/rdbms/%s/logs?tail=%s&follow=true", c.config.DaemonURL, name, tail)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.config.AuthToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return fmt.Errorf("%s", errResp.Error)
		}
		return fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	_, err = io.Copy(c.out, resp.Body)
	return err
}

func (c *Client) applyAction(ctx context.Context, cmd *cli.Command) error {
	instanceName := cmd.Args().First()
	if instanceName == "" {
		return fmt.Errorf("instance name is required\n\nUsage: oddk instance apply <instance-name> --parameter-group <group>")
	}

	parameterGroup := cmd.String("parameter-group")

	reqBody := map[string]string{
		"parameterGroup": parameterGroup,
	}

	_, _ = fmt.Fprintf(c.out, "Reconfiguring instance %s with parameter group %s...\n", instanceName, parameterGroup)

	respBody, err := c.request("PUT", "/api/rdbms/"+instanceName+"/config", reqBody)
	if err != nil {
		return err
	}

	var instance struct {
		Name           string `json:"name"`
		ParameterGroup string `json:"parameterGroup"`
		Status         string `json:"status"`
	}
	if err := json.Unmarshal(respBody, &instance); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "Instance %s reconfigured successfully.\n", instance.Name)
	_, _ = fmt.Fprintf(c.out, "Parameter group: %s\n", instance.ParameterGroup)
	_, _ = fmt.Fprintf(c.out, "Status: %s\n", instance.Status)

	return nil
}

func (c *Client) switchAction(ctx context.Context, cmd *cli.Command) error {
	instanceName, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	image := cmd.String("image")
	version := cmd.String("version")

	// Auto-provision the target image (pull only if missing, with live
	// progress) so switch works without a separate 'oddk pull' first.
	if err := c.ensureImage(ctx, version, image); err != nil {
		return err
	}

	reqBody := map[string]string{
		"image":   image,
		"version": version,
	}

	_, _ = fmt.Fprintf(c.out, "Switching instance %s to image %s...\n", instanceName, image)

	respBody, err := c.request("PUT", "/api/rdbms/"+instanceName+"/image", reqBody)
	if err != nil {
		return err
	}

	var instance struct {
		Name    string `json:"name"`
		Image   string `json:"image"`
		Version string `json:"version"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(respBody, &instance); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "Instance %s switched successfully.\n", instance.Name)
	_, _ = fmt.Fprintf(c.out, "Image: %s\n", instance.Image)
	_, _ = fmt.Fprintf(c.out, "Version: %s\n", instance.Version)
	_, _ = fmt.Fprintf(c.out, "Status: %s\n", instance.Status)

	return nil
}

func (c *Client) updateAction(ctx context.Context, cmd *cli.Command) error {
	instanceName, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	// image is optional: empty means "re-pull the instance's current tag".
	body := map[string]string{"image": cmd.String("image")}

	return c.streamProgress(ctx, "POST", "/api/rdbms/"+instanceName+"/update", body)
}

func (c *Client) majorUpgradeAction(ctx context.Context, cmd *cli.Command) error {
	instanceName, err := requireInstanceName(cmd)
	if err != nil {
		return err
	}

	targetVersion := cmd.String("target-version")
	image := cmd.String("image")

	if !cmd.Bool("yes") {
		_, _ = fmt.Fprintf(c.out, "About to upgrade instance '%s' to PostgreSQL %s.\n\n", instanceName, targetVersion)
		_, _ = fmt.Fprintln(c.out, "This is a major-version upgrade performed via dump/restore:")
		_, _ = fmt.Fprintln(c.out, "  - The instance will be DOWN for the duration of the restore (can be long for large databases).")
		_, _ = fmt.Fprintln(c.out, "  - Quiesce your application first: writes made after the upgrade starts are NOT migrated.")
		_, _ = fmt.Fprintln(c.out, "  - A full backup is taken first and used as the rollback artifact.")
		_, _ = fmt.Fprintln(c.out, "  - While it runs, ALL instances' health checks are paused and other operations (incl. scheduled backups) are blocked.")
		_, _ = fmt.Fprintln(c.out)

		confirmed, err := c.cliConfirm(fmt.Sprintf("Proceed with upgrading '%s' to PostgreSQL %s? [y/N]: ", instanceName, targetVersion))
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(c.out, "Cancelled")
			return nil
		}
	}

	reqBody := map[string]string{
		"targetVersion": targetVersion,
		"image":         image,
	}

	_, _ = fmt.Fprintf(c.out, "Upgrading instance %s to PostgreSQL %s (this may take a while)...\n", instanceName, targetVersion)

	respBody, err := c.request("POST", "/api/rdbms/"+instanceName+"/major-upgrade", reqBody)
	if err != nil {
		return err
	}

	var result struct {
		Instance struct {
			Name    string `json:"name"`
			Image   string `json:"image"`
			Version string `json:"version"`
			Status  string `json:"status"`
		} `json:"instance"`
		FromVersion       string `json:"fromVersion"`
		ToVersion         string `json:"toVersion"`
		BackupID          int    `json:"backupId"`
		DatabasesRestored int    `json:"databasesRestored"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "Instance %s upgraded successfully.\n", result.Instance.Name)
	_, _ = fmt.Fprintf(c.out, "PostgreSQL: %s -> %s\n", result.FromVersion, result.ToVersion)
	_, _ = fmt.Fprintf(c.out, "Image: %s\n", result.Instance.Image)
	_, _ = fmt.Fprintf(c.out, "Status: %s\n", result.Instance.Status)
	_, _ = fmt.Fprintf(c.out, "Databases restored: %d\n", result.DatabasesRestored)
	_, _ = fmt.Fprintf(c.out, "Pre-upgrade backup ID: %d\n", result.BackupID)

	return nil
}

// runDaemon starts the daemon
func runDaemon(port int, dataDir, backupDir string, allowRemote bool) error {
	server, err := daemon.NewServer(port, dataDir, backupDir, 70, allowRemote) // 70s health check interval
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	// Note: the daemon no longer drops a .oddk-cli.json in its working dir.
	// Mint and install a CLI token explicitly with `oddk auth mint` instead.
	return server.Start()
}

// openAuthStore resolves the data dir (the same way the daemon does for the
// oddk user, using the passwd home rather than $HOME, which sudo may leave as
// the caller's) and opens oddk.db for the narrow purpose of managing auth
// tokens. The caller must Close the returned db. All `auth` subcommands run
// locally against the database, not the daemon HTTP API.
func openAuthStore(cmd *cli.Command) (*auth.AuthStore, *sqlx.DB, error) {
	dataDir := cmd.String("data-dir")
	if dataDir == "" {
		u, err := user.Current()
		if err != nil {
			return nil, nil, fmt.Errorf("determine current user: %w", err)
		}
		if u.Username == "oddk" {
			dataDir = filepath.Join(u.HomeDir, "data")
		} else {
			return nil, nil, fmt.Errorf("--data-dir is required when not running as the oddk user")
		}
	}

	// Safeguard: the auth commands open SQLite directly and create WAL/SHM
	// sidecar files in the data dir, so the invoking user must own it. The usual
	// mistake is forgetting `sudo -u oddk`, which would otherwise surface as a
	// confusing low-level permission/IO error (or leave daemon-unreadable files
	// behind). Fail clearly and tell them which user to become.
	if err := ensureDataDirOwnedByCurrentUser(dataDir); err != nil {
		return nil, nil, err
	}

	dbPath := filepath.Join(dataDir, "oddk.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil, fmt.Errorf("database not found at %s - is ODDK installed and has the daemon started? (%w)", dbPath, err)
	}

	return store.OpenAuthOnly(dbPath)
}

// ensureDataDirOwnedByCurrentUser fails if the data dir is owned by a different
// user than the one running the command - almost always a forgotten
// `sudo -u oddk`. A missing data dir is left to the db-existence check below,
// and non-unix stat is skipped (can't determine ownership).
func ensureDataDirOwnedByCurrentUser(dataDir string) error {
	info, err := os.Stat(dataDir)
	if err != nil {
		return nil // missing/unreadable: reported by the db-existence check
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	ownerUID := int(stat.Uid)
	if ownerUID == os.Getuid() {
		return nil
	}
	owner := uidName(ownerUID)
	return fmt.Errorf("data directory %s is owned by %s but you are running as %s; run the auth commands as that user, e.g. sudo -u %s oddk auth mint",
		dataDir, owner, uidName(os.Getuid()), owner)
}

// uidName resolves a numeric uid to a username, falling back to the number.
func uidName(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username
	}
	return strconv.Itoa(uid)
}

// authMintAction mints a fresh CLI auth token. By default it emits shell
// commands that install ~/.config/oddk/cli.json for the user running that
// shell, meant to be run as the oddk service user and eval'd by an admin:
//
//	eval "$(sudo -u oddk /usr/local/bin/oddk auth mint)"
//
// With --json it just prints the CLI config JSON to stdout.
//
// Tokens are stored hashed, so an existing token's plaintext can't be reissued;
// each invocation mints a new (independently valid) token.
func authMintAction(_ context.Context, cmd *cli.Command) error {
	port := cmd.Int("port")
	asJSON := cmd.Bool("json")

	authStore, db, err := openAuthStore(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	token, err := authStore.CreateToken()
	if err != nil {
		return fmt.Errorf("mint auth token (is the daemon initialized?): %w", err)
	}

	configJSON, err := json.MarshalIndent(map[string]string{
		"daemonUrl": fmt.Sprintf("http://localhost:%d", port),
		"authToken": token,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if asJSON {
		_, _ = fmt.Fprintf(os.Stdout, "%s\n", configJSON)
		return nil
	}

	// Emit shell that writes the config in the *caller's* shell ($HOME, perms).
	// Designed for `eval "$(sudo -u oddk /usr/local/bin/oddk auth mint)"`.
	_, _ = fmt.Fprintf(os.Stdout,
		"mkdir -p \"$HOME/.config/oddk\"\n"+
			"umask 077\n"+
			"cat > \"$HOME/.config/oddk/cli.json\" <<'ODDK_CLI_CONFIG_EOF'\n"+
			"%s\n"+
			"ODDK_CLI_CONFIG_EOF\n"+
			"chmod 600 \"$HOME/.config/oddk/cli.json\"\n"+
			"echo \"ODDK CLI configured: $HOME/.config/oddk/cli.json\"\n",
		configJSON)

	// If stdout is a terminal the user ran this raw (not via eval); nudge them.
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		_, _ = fmt.Fprintln(os.Stderr,
			"\n# Minted a new CLI token. Apply it for the current user with:\n"+
				"#   eval \"$(sudo -u oddk /usr/local/bin/oddk auth mint)\"\n"+
				"# (or use --json to print the config)")
	}
	return nil
}

// authListAction lists the metadata of all stored auth tokens.
func authListAction(_ context.Context, cmd *cli.Command) error {
	authStore, db, err := openAuthStore(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	tokens, err := authStore.ListTokens()
	if err != nil {
		return err
	}

	if len(tokens) == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "No auth tokens.")
		return nil
	}

	_, _ = fmt.Fprintf(os.Stdout, "%-6s  %-10s  %s\n", "ID", "PREFIX", "CREATED")
	for _, t := range tokens {
		_, _ = fmt.Fprintf(os.Stdout, "%-6d  %-10s  %s\n", t.ID, t.TokenPrefix, t.CreatedAt)
	}
	return nil
}

// authDeleteAction revokes a single auth token by id.
func authDeleteAction(_ context.Context, cmd *cli.Command) error {
	idArg := cmd.Args().First()
	if idArg == "" {
		return fmt.Errorf("token id is required: oddk auth delete <id>")
	}
	id, err := strconv.ParseInt(idArg, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid token id %q: must be an integer", idArg)
	}

	authStore, db, err := openAuthStore(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := authStore.DeleteToken(id); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(os.Stdout, "Deleted auth token %d.\n", id)
	return nil
}
