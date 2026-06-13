package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/hypersequent/oddk/internal/daemon"
	"github.com/hypersequent/oddk/internal/store"
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

	resp, err := c.request("POST", "/api/pull", req)
	if err != nil {
		return err
	}

	var result struct {
		Version string   `json:"version"`
		Tags    []string `json:"tags"`
		Message string   `json:"message"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
	if len(result.Tags) > 0 {
		_, _ = fmt.Fprintf(c.out, "Available tags: %s\n", strings.Join(result.Tags, ", "))
	}

	return nil
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

	req := map[string]any{
		"databaseName": databaseName,
	}

	resp, err := c.request("POST", fmt.Sprintf("/api/rdbms/%s/databases", instanceName), req)
	if err != nil {
		return err
	}

	var result struct {
		DatabaseName string `json:"databaseName"`
		Message      string `json:"message"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
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
	out, err := exec.CommandContext(probeCtx, dockerPath, "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			detail = "\n  " + strings.ReplaceAll(detail, "\n", "\n  ")
		}
		return "", fmt.Errorf("cannot reach Docker as the current user (%w):%s\n\n"+
			"`oddk instance psql` needs Docker access. Either add your user to the docker group:\n"+
			"  sudo usermod -aG docker $USER   # then log out and back in\n"+
			"or re-run the command with sudo.", err, detail)
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

	// Construct Docker command
	dockerArgs := []string{
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
	}

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

	req := map[string]any{
		"username": username,
		"readOnly": readonly,
		"owner":    owner,
	}

	resp, err := c.request("POST", fmt.Sprintf("/api/rdbms/%s/databases/%s/users", instanceName, database), req)
	if err != nil {
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

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
	_, _ = fmt.Fprintf(c.out, "\nCredentials:\n")
	_, _ = fmt.Fprintf(c.out, "Username: %s\n", result.Username)
	_, _ = fmt.Fprintf(c.out, "Password: %s\n", result.Password)
	_, _ = fmt.Fprintf(c.out, "Database: %s\n", result.Database)
	_, _ = fmt.Fprintf(c.out, "Access: %s\n", map[bool]string{true: "read-only", false: "read-write"}[result.ReadOnly])

	// Get instance details for connection string
	instResp, err := c.request("GET", fmt.Sprintf("/api/rdbms/%s", instanceName), nil)
	if err == nil {
		var instance struct {
			Port int `json:"port"`
		}
		if json.Unmarshal(instResp, &instance) == nil {
			_, _ = fmt.Fprintf(c.out, "\nConnection string:\n")
			_, _ = fmt.Fprintf(c.out, "postgresql://%s:%s@10.88.0.1:%d/%s?sslmode=disable\n",
				result.Username, result.Password, instance.Port, result.Database)
		}
	}

	_, _ = fmt.Fprintf(c.out, "\n⚠️  NOTE: This password is not saved. Save it securely now.\n")
	_, _ = fmt.Fprintf(c.out, "To reset the password later, use: oddk instance reset-db-user-password %s --username %s\n",
		instanceName, result.Username)

	return nil
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

	// Write client config if a new token was created
	if server.HasNewlyCreatedToken() {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current directory: %w", err)
		}
		if err := server.WriteClientConfig(cwd); err != nil {
			return fmt.Errorf("failed to write client config: %w", err)
		}
	}

	return server.Start()
}

// cliAuthAction mints a fresh CLI auth token and emits shell commands that
// install ~/.config/oddk/cli.json for the user running that shell. It is meant
// to be run as the oddk service user (which owns the database) and eval'd by an
// admin, e.g.:  eval "$(sudo -u oddk /usr/local/bin/oddk cli-auth)"
//
// Tokens are stored hashed, so an existing token's plaintext can't be reissued;
// each invocation mints a new (independently valid) token.
func cliAuthAction(_ context.Context, cmd *cli.Command) error {
	port := cmd.Int("port")
	dataDir := cmd.String("data-dir")

	// Resolve the data dir the same way the daemon does for the oddk user,
	// using the passwd home (not $HOME, which sudo may leave as the caller's).
	if dataDir == "" {
		u, err := user.Current()
		if err != nil {
			return fmt.Errorf("determine current user: %w", err)
		}
		if u.Username == "oddk" {
			dataDir = filepath.Join(u.HomeDir, "data")
		} else {
			return fmt.Errorf("--data-dir is required when not running as the oddk user")
		}
	}

	dbPath := filepath.Join(dataDir, "oddk.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("database not found at %s - is ODDK installed and has the daemon started? (%w)", dbPath, err)
	}

	authStore, db, err := store.OpenAuthOnly(dbPath)
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

	// Emit shell that writes the config in the *caller's* shell ($HOME, perms).
	// Designed for `eval "$(sudo -u oddk /usr/local/bin/oddk cli-auth)"`.
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
				"#   eval \"$(sudo -u oddk /usr/local/bin/oddk cli-auth)\"")
	}
	return nil
}
