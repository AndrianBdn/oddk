//go:build oddk_debug

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// testInstanceApply tests applying a new parameter group to an existing instance
func testInstanceApply(h *TestHarness) error {
	// Test 1: Pull image first
	_, err := h.pullImageCLI("16")
	if err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	// Test 2: Create instance with default parameter group
	instanceName := "apply-test-instance"
	instancePort := 15450

	createOutput, err := h.createInstanceCLI(instanceName, instancePort)
	if err != nil {
		return fmt.Errorf("create instance failed: %w", err)
	}

	if !strings.Contains(createOutput, "Created RDBMS instance") {
		return fmt.Errorf("create instance output should indicate success: %s", createOutput)
	}

	// Test 3: Wait for PostgreSQL to be ready
	if err := h.waitForPostgreSQL(instancePort); err != nil {
		return fmt.Errorf("PostgreSQL did not start in time: %w", err)
	}

	// Test 4: Get password for later verification
	passwordOutput, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("get password failed: %w", err)
	}
	password := strings.TrimSpace(passwordOutput)

	// Test 4b: the freshly created container must not carry the instance's real
	// password in its Docker config, which Docker keeps for the container's
	// whole lifetime. initdb needs *a* password, so POSTGRES_PASSWORD is
	// present — but it must be the throwaway create swapped out, not the
	// credential the operator was just handed.
	created, err := h.inspectContainer(context.Background(), "oddk-pg-"+instanceName)
	if err != nil {
		return fmt.Errorf("inspect container after create: %w", err)
	}
	if strings.Contains(strings.Join(created.Config.Env, "\n"), password) {
		return fmt.Errorf("created container's Config.Env leaks the instance password: %v", created.Config.Env)
	}
	var envPassword string
	for _, e := range created.Config.Env {
		if v, ok := strings.CutPrefix(e, "POSTGRES_PASSWORD="); ok {
			envPassword = v
		}
	}
	if envPassword == "" {
		return fmt.Errorf("expected initdb's throwaway in Config.Env, got %v", created.Config.Env)
	}
	// And it must be DEAD. Asserting only that the real password is absent
	// would still pass if the swap had silently left a working credential
	// behind; this is what proves the visible value authenticates nothing.
	deadStr := fmt.Sprintf("postgresql://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable", envPassword, instancePort)
	deadDB, err := sql.Open("postgres", deadStr)
	if err != nil {
		return fmt.Errorf("open connection with the throwaway: %w", err)
	}
	pingErr := deadDB.Ping()
	_ = deadDB.Close()
	if pingErr == nil {
		return fmt.Errorf("the password left in Config.Env still authenticates: create did not replace initdb's throwaway")
	}

	// Test 5: Verify initial max_connections (from default group)
	connStr := fmt.Sprintf("postgresql://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable", password, instancePort)
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("open database connection: %w", err)
	}

	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	var initialMaxConn string
	err = db.QueryRow("SHOW max_connections").Scan(&initialMaxConn)
	if err != nil {
		return fmt.Errorf("query initial max_connections: %w", err)
	}
	_ = db.Close()

	// Test 6: Create a custom parameter group with different max_connections
	testParamFile := filepath.Join(h.dataDir, "apply-test-params.json")
	testParams := []map[string]any{
		{
			"name":      "max_connections",
			"type":      "postgres_cli_arg",
			"valueType": "numeric",
			"value":     "75",
		},
		{
			"name":      "shared_buffers",
			"type":      "postgres_cli_arg",
			"valueType": "numeric_mem",
			"value":     "128 MB",
		},
	}

	testParamsJSON, err := json.MarshalIndent(testParams, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal test parameters: %w", err)
	}

	if err := os.WriteFile(testParamFile, testParamsJSON, 0o600); err != nil {
		return fmt.Errorf("write test parameter file: %w", err)
	}

	// Oversized lock table: must be refused BEFORE the working container is
	// destroyed (the 2026-07-29 apply outage). 1024 MB RAM cannot fit this.
	oversizedFile := filepath.Join(h.dataDir, "apply-oversized-params.json")
	oversized := []map[string]any{
		{"name": "shared_buffers", "type": "postgres_cli_arg", "valueType": "numeric_mem", "value": "128 MB"},
		{"name": "max_connections", "type": "postgres_cli_arg", "valueType": "numeric", "value": "512"},
		{"name": "max_locks_per_transaction", "type": "postgres_cli_arg", "valueType": "numeric", "value": "65536"},
	}
	oversizedJSON, err := json.MarshalIndent(oversized, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal oversized parameters: %w", err)
	}
	if err := os.WriteFile(oversizedFile, oversizedJSON, 0o600); err != nil {
		return fmt.Errorf("write oversized parameter file: %w", err)
	}
	if _, err := h.createParameterGroupCLI("apply-oversized-group", oversizedFile); err != nil {
		return fmt.Errorf("create oversized parameter group failed: %w", err)
	}
	_, applyErr := h.applyParameterGroupCLI(instanceName, "apply-oversized-group")
	if applyErr == nil {
		return fmt.Errorf("applying an oversized parameter group should fail before destroying the container")
	}
	if !strings.Contains(applyErr.Error(), "shared memory") && !strings.Contains(applyErr.Error(), "RAM") {
		return fmt.Errorf("oversized apply error should mention shared memory vs RAM, got: %v", applyErr)
	}
	if err := h.waitForPostgreSQL(instancePort); err != nil {
		return fmt.Errorf("instance should still be reachable after refused apply: %w", err)
	}
	// A preflight refusal must also put the status back: the operation set
	// "reconfiguring" before calling Docker, and leaving it there would make
	// reconcile turn a perfectly healthy instance into "error" on next restart.
	statusOutput, err := h.getInstanceStatusCLI(instanceName)
	if err != nil {
		return fmt.Errorf("get status after refused apply: %w", err)
	}
	if !strings.Contains(statusOutput, "running") {
		return fmt.Errorf("instance should still be 'running' after a refused apply, got: %s", statusOutput)
	}
	if _, err := h.deleteParameterGroupCLI("apply-oversized-group", true); err != nil {
		return fmt.Errorf("delete oversized parameter group: %w", err)
	}

	createGroupOutput, err := h.createParameterGroupCLI("apply-test-group", testParamFile)
	if err != nil {
		return fmt.Errorf("create parameter group failed: %w", err)
	}

	if !strings.Contains(createGroupOutput, "created successfully") {
		return fmt.Errorf("create parameter group output should indicate success: %s", createGroupOutput)
	}

	// Test 7: Apply the new parameter group to the instance
	applyOutput, err := h.applyParameterGroupCLI(instanceName, "apply-test-group")
	if err != nil {
		return fmt.Errorf("apply parameter group failed: %w", err)
	}

	if !strings.Contains(applyOutput, "reconfigured successfully") {
		return fmt.Errorf("apply output should indicate success: %s", applyOutput)
	}

	if !strings.Contains(applyOutput, "apply-test-group") {
		return fmt.Errorf("apply output should show new parameter group: %s", applyOutput)
	}

	// Test 8: Wait for PostgreSQL to be ready again after reconfiguration
	if err := h.waitForPostgreSQL(instancePort); err != nil {
		return fmt.Errorf("PostgreSQL did not restart in time after apply: %w", err)
	}

	// Test 8b: the recreated container must NOT carry the postgres password in
	// its Docker config. A recreate reuses a populated volume, so the
	// entrypoint skips initdb and discards POSTGRES_PASSWORD — but Docker keeps
	// Config.Env for the container's lifetime and hands it to anything that can
	// read container metadata. This is the assertion that catches a
	// reintroduction: inspecting the real container is the only thing that
	// proves what shipped, exactly as reading the raw column is for an
	// encrypted-at-rest check.
	inspected, err := h.inspectContainer(context.Background(), "oddk-pg-"+instanceName)
	if err != nil {
		return fmt.Errorf("inspect container after apply: %w", err)
	}
	for _, env := range inspected.Config.Env {
		if strings.HasPrefix(env, "POSTGRES_PASSWORD") {
			return fmt.Errorf("recreated container must not carry POSTGRES_PASSWORD in Config.Env, found %q", env)
		}
	}
	if strings.Contains(strings.Join(inspected.Config.Env, "\n"), password) {
		return fmt.Errorf("recreated container's Config.Env leaks the postgres password: %v", inspected.Config.Env)
	}

	// Test 9: Verify max_connections changed to 75
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("reopen database connection: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping database after apply: %w", err)
	}

	var newMaxConn string
	err = db.QueryRow("SHOW max_connections").Scan(&newMaxConn)
	if err != nil {
		return fmt.Errorf("query new max_connections: %w", err)
	}

	if newMaxConn != "75" {
		return fmt.Errorf("expected max_connections to be 75 after apply, got %s (was %s before)", newMaxConn, initialMaxConn)
	}

	// Test 10: Verify shared_buffers also changed
	var newSharedBuffers string
	err = db.QueryRow("SHOW shared_buffers").Scan(&newSharedBuffers)
	if err != nil {
		return fmt.Errorf("query new shared_buffers: %w", err)
	}

	if newSharedBuffers != "128MB" {
		return fmt.Errorf("expected shared_buffers to be 128MB after apply, got %s", newSharedBuffers)
	}

	// Test 11: Verify instance list shows new parameter group
	listOutput, err := h.listInstancesCLI()
	if err != nil {
		return fmt.Errorf("list instances failed: %w", err)
	}

	if !strings.Contains(listOutput, "apply-test-group") {
		return fmt.Errorf("instance list should show new parameter group: %s", listOutput)
	}

	// Test 12: Try to apply the same parameter group (should fail)
	_, err = h.applyParameterGroupCLI(instanceName, "apply-test-group")
	if err == nil {
		return fmt.Errorf("applying same parameter group should fail")
	}

	if !strings.Contains(err.Error(), "already uses parameter group") {
		return fmt.Errorf("error should mention 'already uses parameter group': %v", err)
	}

	// Test 13: Try to apply non-existent parameter group (should fail)
	_, err = h.applyParameterGroupCLI(instanceName, "non-existent-group")
	if err == nil {
		return fmt.Errorf("applying non-existent parameter group should fail")
	}

	// Test 13b: resize and move the port in place. The data must survive, the
	// container must carry the new limits and binding, and the row must record
	// the new shape while keeping the parameter group applied above.
	newPort := instancePort + 1
	probeDSN := func(port int) string {
		return fmt.Sprintf("postgresql://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable", password, port)
	}
	if err := execSQL(probeDSN(instancePort),
		"CREATE TABLE reconfigure_probe(v text)", "INSERT INTO reconfigure_probe VALUES ('kept')"); err != nil {
		return fmt.Errorf("write probe row before reconfigure: %w", err)
	}
	out, err := h.runCLI("instance", "apply", instanceName, "--ram", "1536M", "--port", strconv.Itoa(newPort))
	if err != nil {
		return fmt.Errorf("apply --ram --port failed: %w", err)
	}
	if !strings.Contains(out, "reconfigured successfully") || !strings.Contains(out, fmt.Sprintf("port %d", newPort)) {
		return fmt.Errorf("apply output should report success on the new port: %s", out)
	}
	if err := h.waitForPostgreSQL(newPort); err != nil {
		return fmt.Errorf("PostgreSQL did not come up on the new port: %w", err)
	}
	if conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("10.88.0.1:%d", instancePort), time.Second); dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("old port %d still answers after the move", instancePort)
	}
	var row struct {
		Port           int    `json:"port"`
		CPUCores       int    `json:"cpuCores"`
		RAMMB          int    `json:"ramMb"`
		ParameterGroup string `json:"parameterGroup"`
		Status         string `json:"status"`
	}
	if code, body, reqErr := h.request("GET", "/api/rdbms/"+instanceName, nil); reqErr != nil || code != 200 {
		return fmt.Errorf("get instance after reconfigure: code %d err %v", code, reqErr)
	} else if err := json.Unmarshal(body, &row); err != nil {
		return fmt.Errorf("parse instance after reconfigure: %w", err)
	}
	if row.Port != newPort || row.RAMMB != 1536 || row.CPUCores != 1 || row.ParameterGroup != "apply-test-group" || row.Status != "running" {
		return fmt.Errorf("instance row after reconfigure = %+v, want port %d, ram 1536, cpu 1, group apply-test-group, running", row, newPort)
	}
	resized, err := h.inspectContainer(context.Background(), "oddk-pg-"+instanceName)
	if err != nil {
		return fmt.Errorf("inspect container after reconfigure: %w", err)
	}
	if want := int64(1536) << 20; resized.HostConfig.Memory != want || resized.HostConfig.ShmSize != want/4 {
		return fmt.Errorf("container limits after reconfigure: memory %d shm %d, want %d / %d",
			resized.HostConfig.Memory, resized.HostConfig.ShmSize, want, want/4)
	}
	boundPorts := map[string]bool{}
	for _, bindings := range resized.HostConfig.PortBindings {
		for _, b := range bindings {
			boundPorts[b.HostPort] = true
		}
	}
	if !boundPorts[strconv.Itoa(newPort)] || boundPorts[strconv.Itoa(instancePort)] {
		return fmt.Errorf("container port bindings after reconfigure = %v, want only %d", boundPorts, newPort)
	}
	if got, err := querySQL(probeDSN(newPort), "SELECT v FROM reconfigure_probe"); err != nil || got != "kept" {
		return fmt.Errorf("probe row after reconfigure = %q, err %v; the data volume must survive a resize", got, err)
	}

	// Test 13c: a refused change costs nothing. Both refusals happen before the
	// running container is touched: a size the host cannot honour, and a port
	// something on the host already answers on (a listener on the gateway,
	// exactly what a stray process would look like).
	if _, err := h.runCLI("instance", "apply", instanceName, "--cpu", "100000"); err == nil {
		return fmt.Errorf("apply --cpu 100000 should be refused")
	} else if !strings.Contains(err.Error(), "CPU cores") {
		return fmt.Errorf("refusal should name the CPU bound: %v", err)
	}
	squatter, err := net.Listen("tcp", fmt.Sprintf("10.88.0.1:%d", newPort+1))
	if err != nil {
		return fmt.Errorf("listen on the gateway to simulate a port squatter: %w", err)
	}
	_, err = h.runCLI("instance", "apply", instanceName, "--port", strconv.Itoa(newPort+1))
	_ = squatter.Close()
	if err == nil {
		return fmt.Errorf("apply --port onto a listening port should be refused")
	} else if !strings.Contains(err.Error(), "already in use on this host") {
		return fmt.Errorf("refusal should say the host port is taken: %v", err)
	}
	if code, body, reqErr := h.request("GET", "/api/rdbms/"+instanceName, nil); reqErr != nil || code != 200 {
		return fmt.Errorf("get instance after refused reconfigure: code %d err %v", code, reqErr)
	} else if err := json.Unmarshal(body, &row); err != nil {
		return fmt.Errorf("parse instance after refused reconfigure: %w", err)
	}
	if row.Port != newPort || row.RAMMB != 1536 || row.Status != "running" {
		return fmt.Errorf("a refused reconfigure changed the row: %+v", row)
	}
	if got, err := querySQL(probeDSN(newPort), "SELECT v FROM reconfigure_probe"); err != nil || got != "kept" {
		return fmt.Errorf("instance must keep serving after a refused reconfigure: %q, %v", got, err)
	}

	// Test 13d: no flags at all is refused, not silently a no-op.
	if _, err := h.runCLI("instance", "apply", instanceName); err == nil || !strings.Contains(err.Error(), "nothing to change") {
		return fmt.Errorf("apply with no flags should be refused with 'nothing to change': %v", err)
	}

	// Test 14: Clean up
	_, err = h.stopInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("stop instance failed: %w", err)
	}

	err = h.destroyInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	_, err = h.deleteParameterGroupCLI("apply-test-group", true)
	if err != nil {
		return fmt.Errorf("delete parameter group failed: %w", err)
	}

	return nil
}

// execSQL runs statements against dsn with the lib/pq driver, one connection,
// stopping at the first failure.
func execSQL(dsn string, statements ...string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}

// querySQL runs a single-value query against dsn.
func querySQL(dsn, query string) (string, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	var v string
	if err := db.QueryRow(query).Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}
