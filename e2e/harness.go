package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/hypersequent/oddk/internal/cli"
	"github.com/hypersequent/oddk/internal/daemon"
)

const (
	testPrefix = "oddk-danger-funct"
	testPort   = 15442 // Use a different port to avoid conflicts
)

type TestHarness struct {
	testName   string
	dataDir    string
	server     *daemon.Server
	authToken  string
	baseURL    string
	docker     *client.Client
	httpClient *http.Client
	env        []string // Environment variables for CLI commands
	fakeS3     *http.Server
	fakeS3Port int
	fakeS3URL  string
}

func setupTestHarness(testName string, kvMap map[string]string, runFakeS3 bool) *TestHarness {
	tempDir, err := os.MkdirTemp("", fmt.Sprintf("oddk-e2e-%s-*", testName))
	if err != nil {
		panic(fmt.Sprintf("Failed to create temp dir: %v", err))
	}

	backupDir := filepath.Join(tempDir, "backups")
	server, err := daemon.NewServer(testPort, tempDir, backupDir, 2, false) // 2s health check interval, loopback bind
	if err != nil {
		_ = os.RemoveAll(tempDir)
		panic(fmt.Sprintf("Failed to create server: %v", err))
	}

	// Set any provided KV values before starting the server
	for key, value := range kvMap {
		if err := server.DebugSetRawKV(key, value); err != nil {
			_ = os.RemoveAll(tempDir)
			panic(fmt.Sprintf("Failed to set KV %s=%s: %v", key, value, err))
		}
	}

	// Write client config to temp dir (should always create new token with fresh temp dir)
	if err := server.WriteClientConfig(tempDir); err != nil {
		_ = os.RemoveAll(tempDir)
		panic(fmt.Sprintf("Failed to write client config: %v", err))
	}

	go func() {
		if err := server.Start(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Server error: %v\n", err)
		}
	}()

	// Wait for server to be ready
	if err := waitForTCP(fmt.Sprintf("localhost:%d", testPort), 3*time.Second); err != nil {
		_ = os.RemoveAll(tempDir)
		panic(fmt.Sprintf("Server didn't start in time: %v", err))
	}

	// Read the generated auth token from temp directory
	tokenFile := filepath.Join(tempDir, ".oddk-cli.json")
	tokenData, err := os.ReadFile(tokenFile) // #nosec G304 - tokenFile is controlled test path
	if err != nil {
		panic(fmt.Sprintf("Failed to read auth token from %s: %v", tokenFile, err))
	}

	var config struct {
		AuthToken string `json:"authToken"`
	}
	if err := json.Unmarshal(tokenData, &config); err != nil {
		panic(fmt.Sprintf("Failed to parse auth token: %v", err))
	}

	dockerClient, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		panic(fmt.Sprintf("Failed to create Docker client: %v", err))
	}

	configPath := filepath.Join(tempDir, ".oddk-cli.json")
	env := []string{
		fmt.Sprintf("ODDK_CLI_CONFIG=%s", configPath),
		fmt.Sprintf("HOME=%s", tempDir), // Prevent reading from real home directory
	}

	harness := &TestHarness{
		testName:   testName,
		dataDir:    tempDir,
		server:     server,
		authToken:  config.AuthToken,
		baseURL:    fmt.Sprintf("http://localhost:%d", testPort),
		docker:     dockerClient,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		env:        env,
	}

	// Setup fake S3 server if requested
	if runFakeS3 {
		harness.setupFakeS3()
	}

	return harness
}

func (h *TestHarness) setupFakeS3() {
	// Find a free port and keep the listener
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("Failed to find free port for fake S3: %v", err))
	}
	port := listener.Addr().(*net.TCPAddr).Port

	backend := s3mem.New()
	faker := gofakes3.New(backend, gofakes3.WithAutoBucket(true))

	server := &http.Server{
		Handler:           faker.Server(),
		ReadHeaderTimeout: 10 * time.Second, // Prevent slowloris attacks
	}

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Printf("FakeS3 server error: %v\n", err)
		}
	}()

	// Wait for server to be ready (should be immediate since listener is already bound)
	if err := waitForTCP(fmt.Sprintf("localhost:%d", port), 3*time.Second); err != nil {
		panic(fmt.Sprintf("FakeS3 server didn't start in time: %v", err))
	}

	h.fakeS3 = server
	h.fakeS3Port = port
	h.fakeS3URL = fmt.Sprintf("http://localhost:%d", port)
}

// restartDaemon shuts the in-process daemon down and starts a fresh one on the
// same data and backup directories, simulating a daemon restart (crash
// recovery, host reboot). Startup reconciliation runs inside daemon.NewServer.
func (h *TestHarness) restartDaemon() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown daemon: %w", err)
	}

	backupDir := filepath.Join(h.dataDir, "backups")
	server, err := daemon.NewServer(testPort, h.dataDir, backupDir, 2, false)
	if err != nil {
		return fmt.Errorf("recreate daemon: %w", err)
	}
	h.server = server

	go func() {
		if err := server.Start(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Server error after restart: %v\n", err)
		}
	}()

	if err := waitForTCP(fmt.Sprintf("localhost:%d", testPort), 5*time.Second); err != nil {
		return fmt.Errorf("daemon didn't come back after restart: %w", err)
	}
	return nil
}

func (h *TestHarness) cleanup() {
	if h.fakeS3 != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.fakeS3.Shutdown(ctx)
	}

	if h.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.server.Shutdown(ctx)
	}

	h.cleanupTestContainersAndVolumes(false) // false = don't print "Cleaned up" messages

	if h.dataDir != "" {
		_ = os.RemoveAll(h.dataDir)
	}

	_ = os.Remove(".oddk-cli.json")
}

func (h *TestHarness) cleanupTestContainersAndVolumes(verbose bool) {
	ctx := context.Background()

	// List containers with our test prefix
	containers, err := h.docker.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("name", testPrefix),
		),
	})
	if err != nil {
		fmt.Printf("Warning: Failed to list containers for cleanup: %v\n", err)
		return
	}

	for _, c := range containers {
		// Force remove container
		if err := h.docker.ContainerRemove(ctx, c.ID, container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		}); err != nil {
			fmt.Printf("Warning: Failed to remove container %s: %v\n", c.Names[0], err)
		} else if verbose {
			fmt.Printf("Cleaned up container: %s\n", c.Names[0])
		}
	}

	// List all volumes
	volumes, err := h.docker.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		fmt.Printf("Warning: Failed to list volumes for cleanup: %v\n", err)
		return
	}

	for _, vol := range volumes.Volumes {
		if strings.Contains(vol.Name, testPrefix) {
			if err := h.docker.VolumeRemove(ctx, vol.Name, true); err != nil {
				fmt.Printf("Warning: Failed to remove volume %s: %v\n", vol.Name, err)
			} else if verbose {
				fmt.Printf("Cleaned up volume: %s\n", vol.Name)
			}
		}
	}
}

// cleanupAllTestContainers is a standalone function for global cleanup
func cleanupAllTestContainers() error {
	dockerClient, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("failed to create Docker client: %w", err)
	}

	h := &TestHarness{
		docker: dockerClient,
	}

	h.cleanupTestContainersAndVolumes(true) // true = print "Cleaned up" messages
	return nil
}

func (h *TestHarness) request(method, path string, body any) (int, []byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(data)
	}

	req, err := http.NewRequest(method, h.baseURL+path, reqBody)
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+h.authToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}

	return resp.StatusCode, respBody, nil
}

func (h *TestHarness) deleteInstance(name string) error {
	status, body, err := h.request("DELETE", "/api/rdbms/"+name, nil)
	if err != nil {
		return fmt.Errorf("delete request failed: %w", err)
	}

	if status != http.StatusOK {
		return fmt.Errorf("expected status 200, got %d: %s", status, body)
	}

	return nil
}

// CLI wrapper methods
func (h *TestHarness) runCLI(args ...string) (string, error) {
	var buf bytes.Buffer
	fullArgs := append([]string{"oddk"}, args...)
	err := cli.Run(fullArgs, h.env, &buf)
	return buf.String(), err
}

func (h *TestHarness) pullImageCLI(version string) (string, error) {
	return h.runCLI("pull", "--version", version)
}

func (h *TestHarness) createInstanceCLI(name string, port int) (string, error) {
	return h.runCLI("create", "--name", name, "--port", fmt.Sprintf("%d", port), "--version", "16", "--cpu", "1", "--ram", "1024M")
}

func (h *TestHarness) createInstanceWithParameterGroupCLI(name string, port int, parameterGroup string) (string, error) {
	return h.runCLI("create", "--name", name, "--port", fmt.Sprintf("%d", port), "--version", "16", "--cpu", "2", "--ram", "2048M", "--parameter-group", parameterGroup)
}

func (h *TestHarness) listInstancesCLI() (string, error) {
	return h.runCLI("list")
}

func (h *TestHarness) getInstanceStatusCLI(name string) (string, error) {
	return h.runCLI("instance", "status", name)
}

func (h *TestHarness) startInstanceCLI(name string) (string, error) {
	return h.runCLI("instance", "start", name)
}

func (h *TestHarness) stopInstanceCLI(name string) (string, error) {
	return h.runCLI("instance", "stop", name)
}

func (h *TestHarness) listDatabasesCLI(name string) (string, error) {
	return h.runCLI("instance", "list-dbs", name)
}

// Note: destroy requires confirmation, so we'll keep using API for cleanup
func (h *TestHarness) destroyInstanceCLI(name string) error {
	// For testing, we'll use the API to avoid interactive prompts
	return h.deleteInstance(name)
}

// backupInstanceCLI creates a backup of an instance via CLI
func (h *TestHarness) backupInstanceCLI(name string) (string, error) {
	return h.runCLI("backup", "make", name)
}

// listBackupsCLI lists backups for an instance via CLI
func (h *TestHarness) listBackupsCLI(name string) (string, error) {
	return h.runCLI("backup", "list", "--instance", name)
}

// getPasswordCLI gets the password for an instance via CLI
func (h *TestHarness) getPasswordCLI(name, format string) (string, error) {
	if format != "" {
		return h.runCLI("instance", "get-postgres-password", name, format)
	}
	return h.runCLI("instance", "get-postgres-password", name)
}

// setPasswordCLI sets the password for an instance via CLI
func (h *TestHarness) setPasswordCLI(name, password string) (string, error) {
	// Add NEW_PGPASSWORD to environment temporarily
	envWithPassword := make([]string, len(h.env), len(h.env)+1)
	copy(envWithPassword, h.env)
	envWithPassword = append(envWithPassword, fmt.Sprintf("NEW_PGPASSWORD=%s", password))
	var buf bytes.Buffer
	fullArgs := []string{"oddk", "instance", "set-postgres-password", name}
	err := cli.Run(fullArgs, envWithPassword, &buf)
	return buf.String(), err
}

// createDatabaseCLI creates a database via CLI
func (h *TestHarness) createDatabaseCLI(instanceName, databaseName string) (string, error) {
	return h.runCLI("instance", "create-db", instanceName, "--database", databaseName)
}

// addDatabaseUserCLI creates a database user via CLI
func (h *TestHarness) addDatabaseUserCLI(instanceName, username, database string, readonly bool) (string, error) {
	args := []string{"instance", "add-db-user", instanceName, "--username", username, "--database", database}
	if readonly {
		args = append(args, "--readonly")
	}
	return h.runCLI(args...)
}

// deleteDatabaseUserCLI deletes a database user via CLI
func (h *TestHarness) deleteDatabaseUserCLI(instanceName, username string) (string, error) {
	// Add ODDK_SKIP_CONFIRM to environment to skip confirmation prompt
	envWithSkip := make([]string, len(h.env), len(h.env)+1)
	copy(envWithSkip, h.env)
	envWithSkip = append(envWithSkip, "ODDK_SKIP_CONFIRM=1")

	var buf bytes.Buffer
	fullArgs := []string{"oddk", "instance", "delete-db-user", instanceName, "--username", username}
	err := cli.Run(fullArgs, envWithSkip, &buf)
	return buf.String(), err
}

// resetDatabaseUserPasswordCLI resets a database user's password via CLI
func (h *TestHarness) resetDatabaseUserPasswordCLI(instanceName, username string) (string, error) {
	return h.runCLI("instance", "reset-db-user-password", instanceName, "--username", username)
}

// listParameterGroupsCLI lists parameter groups via CLI
func (h *TestHarness) listParameterGroupsCLI() (string, error) {
	return h.runCLI("parameters", "get")
}

// getParameterGroupCLI gets a specific parameter group via CLI
func (h *TestHarness) getParameterGroupCLI(name string) (string, error) {
	return h.runCLI("parameters", "get", "--name", name)
}

// getParameterGroupsJSONCLI gets parameter groups as JSON via CLI
func (h *TestHarness) getParameterGroupsJSONCLI() (string, error) {
	return h.runCLI("parameters", "get", "--json")
}

// createParameterGroupCLI creates a parameter group from JSON file via CLI
func (h *TestHarness) createParameterGroupCLI(name, filePath string) (string, error) {
	return h.runCLI("parameters", "put", name, "--file", filePath)
}

// deleteParameterGroupCLI deletes a parameter group via CLI
func (h *TestHarness) deleteParameterGroupCLI(name string, force bool) (string, error) {
	if force {
		return h.runCLI("parameters", "delete", name, "--force")
	}
	return h.runCLI("parameters", "delete", name)
}

// applyParameterGroupCLI applies a parameter group to an instance via CLI
func (h *TestHarness) applyParameterGroupCLI(instanceName, parameterGroup string) (string, error) {
	return h.runCLI("instance", "apply", instanceName, "--parameter-group", parameterGroup)
}

// pullImageWithImageFlagCLI pulls an image using the --image flag
func (h *TestHarness) pullImageWithImageFlagCLI(image string) (string, error) {
	return h.runCLI("pull", "--image", image)
}

// switchInstanceCLI switches an instance to a different Docker image via CLI
func (h *TestHarness) switchInstanceCLI(instanceName, image string) (string, error) {
	return h.runCLI("instance", "switch", instanceName, "--image", image)
}

// waitForTCP waits for a TCP port to be accessible
func waitForTCP(address string, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s to be accessible", address)
}

// waitForPostgreSQL waits until a PostgreSQL instance actually answers queries
// on the gateway IP 10.88.0.1. A bare TCP check isn't enough: the port opens
// before the server finishes starting up, during which it rejects connections
// with "the database system is starting up" (SQLSTATE 57P03). We poll with a
// real connection attempt (using a deliberately wrong password) and treat an
// auth failure as ready — it means the server is up and serving — while 57P03
// (and the related recovery/shutdown states) and network errors mean keep
// waiting.
func (h *TestHarness) waitForPostgreSQL(port int) error {
	connStr := fmt.Sprintf("postgres://postgres:notthepassword@10.88.0.1:%d/postgres?sslmode=disable", port)
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		conn, err := pgx.Connect(ctx, connStr)
		if err == nil {
			_ = conn.Close(ctx)
			cancel()
			return nil
		}
		cancel()
		lastErr = err

		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "57P03", "57P02", "57P01": // starting up / in recovery / shutting down
				// not ready yet — keep waiting
			default:
				return nil // server answered (e.g. auth failure) => it's up
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for PostgreSQL on 10.88.0.1:%d to accept queries: %w", port, lastErr)
}
