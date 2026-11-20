//go:build oddk_debug

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// testMajorUpgradeRefusesRejectedParameter: a parameter group the target major
// will not start with must stop major-upgrade BEFORE the backup and the volume
// removal. PostgreSQL 19 removed io_workers (a PG 18 setting), and an unknown
// -c setting is FATAL, so before the preflight an 18 instance carrying it lost
// its volume and then failed to start at 19, leaving it in error with only the
// pre-upgrade backup to recover from.
func testMajorUpgradeRefusesRejectedParameter(h *TestHarness) error {
	log.Println("=== Testing major-upgrade refuses a parameter group the target rejects (18 -> 19) ===")

	instanceName := testPrefix + "-pgupgrade-param"
	groupName := "e2e-pg18-only"
	port := 15457

	log.Println("Step 1: Pulling PostgreSQL 18 and 19 images")
	for _, v := range []string{"18", "19"} {
		if _, err := h.pullImageCLI(v); err != nil {
			return fmt.Errorf("pull %s failed: %w", v, err)
		}
	}

	log.Println("Step 2: Creating a group with io_workers (accepted by 18, removed in 19)")
	groupFile := filepath.Join(h.dataDir, "pg18-only-params.json")
	groupJSON, err := json.Marshal([]map[string]string{
		{"name": "max_connections", "type": "postgres_cli_arg", "valueType": "numeric", "value": "100"},
		{"name": "io_workers", "type": "postgres_cli_arg", "valueType": "numeric", "value": "3"},
	})
	if err != nil {
		return fmt.Errorf("marshal group: %w", err)
	}
	if err := os.WriteFile(groupFile, groupJSON, 0o600); err != nil {
		return fmt.Errorf("write group file: %w", err)
	}
	if output, err := h.createParameterGroupCLI(groupName, groupFile); err != nil {
		return fmt.Errorf("create parameter group: %w (output: %s)", err, output)
	}
	defer func() { _, _ = h.deleteParameterGroupCLI(groupName, true) }()

	log.Println("Step 3: Creating a PG18 instance with that group")
	output, err := h.runCLI("create",
		"--name", instanceName, "--version", "18",
		"--port", strconv.Itoa(port), "--cpu", "1", "--ram", "512M",
		"--parameter-group", groupName)
	if err != nil {
		return fmt.Errorf("create instance failed: %w (output: %s)", err, output)
	}
	defer func() { _ = h.destroyInstanceCLI(instanceName) }()
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}
	if output, err := h.createDatabaseCLI(instanceName, "keepme"); err != nil {
		return fmt.Errorf("create database: %w (output: %s)", err, output)
	}
	backupsBefore, err := countInstanceBackups(h, instanceName)
	if err != nil {
		return err
	}

	log.Println("Step 4: major-upgrade to 19 must be refused, naming the setting")
	_, err = h.runCLI("instance", "major-upgrade", instanceName, "--target-version", "19", "--yes")
	if err == nil {
		return fmt.Errorf("expected major-upgrade to be refused for a group PG 19 rejects")
	}
	for _, want := range []string{`unrecognized configuration parameter "io_workers"`, groupName, "nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("refusal should contain %q, got: %v", want, err)
		}
	}

	log.Println("Step 5: the API refusal is a 400")
	status, body, err := h.request("POST", "/api/rdbms/"+instanceName+"/major-upgrade",
		map[string]string{"targetVersion": "19"})
	if err != nil {
		return fmt.Errorf("major-upgrade request: %w", err)
	}
	if status != http.StatusBadRequest || !strings.Contains(string(body), "io_workers") {
		return fmt.Errorf("expected 400 naming io_workers, got %d: %s", status, body)
	}

	log.Println("Step 6: the instance is untouched: running, still 18, data present, no backup taken")
	_, body, err = h.request("GET", "/api/rdbms/"+instanceName, nil)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}
	var inst struct {
		Version string `json:"version"`
		Image   string `json:"image"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &inst); err != nil {
		return fmt.Errorf("unmarshal instance: %w", err)
	}
	if inst.Version != "18" || inst.Image != "postgres:18" || inst.Status != "running" {
		return fmt.Errorf("instance changed by a refused upgrade: version=%q image=%q status=%q", inst.Version, inst.Image, inst.Status)
	}
	dbList, err := h.listDatabasesCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list databases: %w", err)
	}
	if !strings.Contains(dbList, "keepme") {
		return fmt.Errorf("database keepme missing after a refused upgrade: %s", dbList)
	}
	backupsAfter, err := countInstanceBackups(h, instanceName)
	if err != nil {
		return err
	}
	if backupsAfter != backupsBefore {
		return fmt.Errorf("a refused upgrade took a backup: %d before, %d after", backupsBefore, backupsAfter)
	}

	log.Println("=== Major-upgrade parameter refusal test PASSED ===")
	return nil
}

func countInstanceBackups(h *TestHarness, instanceName string) (int, error) {
	status, body, err := h.request("GET", "/api/rdbms/"+instanceName+"/backups", nil)
	if err != nil {
		return 0, fmt.Errorf("list backups: %w", err)
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("list backups: status %d: %s", status, body)
	}
	var backups []json.RawMessage
	if err := json.Unmarshal(body, &backups); err != nil {
		return 0, fmt.Errorf("unmarshal backups: %w (body: %s)", err, body)
	}
	return len(backups), nil
}
