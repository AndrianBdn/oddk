package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// testSnapshotPhysicalMake pins the PHYSICAL snapshot format — the default
// since 0.1.61 — on PostgreSQL 18, whose relocated data directory
// (/var/lib/postgresql/18/docker) exercises the restore path's tar re-rooting.
//
// It covers, in order: the physical archive layout (basebackup/ members, no
// logical dumps), the v2 manifest with per-entry format/captureMode and the
// source architecture, a live restore-instance (writes since the snapshot are
// gone), a COLD capture of a stopped instance (which logical mode reduces to
// configuration-only), and the cold restore's contract of coming back
// verified-but-stopped.
func testSnapshotPhysicalMake(h *TestHarness) error {
	log.Println("=== Testing Physical Snapshot (make + restore-instance + cold capture, PG18) ===")

	if _, err := h.pullImageCLI("18"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	const (
		port   = 15501
		dbName = "physdb"
	)
	instanceName := fmt.Sprintf("oddk-danger-funct-phys-%d", time.Now().Unix())

	log.Println("Step 1: Creating a PG18 instance with data")
	output, err := h.runCLI("create",
		"--name", instanceName, "--version", "18",
		"--port", strconv.Itoa(port), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}
	if output, err = h.createDatabaseCLI(instanceName, dbName); err != nil {
		return fmt.Errorf("create database: %w (output: %s)", err, output)
	}
	postgresPassword, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("read postgres password: %w", err)
	}
	postgresPassword = strings.TrimSpace(postgresPassword)

	seedSQL := "CREATE TABLE things (id int primary key); INSERT INTO things VALUES (1),(2),(3);"
	if err := h.execSQLAsUser(port, dbName, "postgres", postgresPassword, seedSQL); err != nil {
		return fmt.Errorf("seed data: %w", err)
	}

	log.Println("Step 2: Taking the DEFAULT snapshot — it must be physical")
	if output, err = h.runCLI("snapshot", "make"); err != nil {
		return fmt.Errorf("snapshot make: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Format: physical") {
		return fmt.Errorf("default snapshot did not report the physical format; output: %s", output)
	}

	backupDir := filepath.Join(h.dataDir, "backups")
	liveArchive, err := newestSnapshotArchive(backupDir)
	if err != nil {
		return err
	}

	log.Println("Step 3: Verifying the physical archive layout and manifest")
	names, err := tarEntryNames(liveArchive)
	if err != nil {
		return fmt.Errorf("read tar entries: %w", err)
	}
	if len(names) == 0 || names[0] != "manifest.json" {
		return fmt.Errorf("manifest.json must stay the FIRST member in physical archives too; order begins: %v",
			names[:min(5, len(names))])
	}
	present := make(map[string]bool, len(names))
	for _, n := range names {
		present[n] = true
	}
	for _, want := range []string{
		"oddk.db",
		filepath.Join("instances", instanceName, "instance.json"),
		filepath.Join("instances", instanceName, "basebackup", "base.tar.zst"),
		filepath.Join("instances", instanceName, "basebackup", "pg_wal.tar"),
		filepath.Join("instances", instanceName, "basebackup", "backup_manifest"),
	} {
		if !present[want] {
			return fmt.Errorf("physical archive is missing %s; members: %v", want, names)
		}
	}
	// The whole point of physical: no logical dumps inside.
	for _, n := range names {
		if strings.Contains(n, "globals.sql") || strings.Contains(n, "databases.json") {
			return fmt.Errorf("physical archive unexpectedly contains a logical member: %s", n)
		}
	}

	raw, err := readTarMember(liveArchive, "manifest.json")
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var manifest snapshotManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.FormatVersion != 2 {
		return fmt.Errorf("physical manifest formatVersion = %d, want 2 (older readers must refuse loudly)", manifest.FormatVersion)
	}
	if manifest.SourceArch == "" {
		return fmt.Errorf("physical manifest records no sourceArch; the cross-arch refusal depends on it")
	}
	if len(manifest.Instances) != 1 {
		return fmt.Errorf("manifest lists %d instances, want 1", len(manifest.Instances))
	}
	entry := manifest.Instances[0]
	if !entry.HasData || entry.Format != "physical" || entry.CaptureMode != "basebackup" {
		return fmt.Errorf("live capture entry = %+v, want hasData/physical/basebackup", entry)
	}

	log.Println("Step 4: snapshot list must show the format")
	listOut, err := h.runCLI("snapshot", "list")
	if err != nil {
		return fmt.Errorf("snapshot list: %w (output: %s)", err, listOut)
	}
	if !strings.Contains(listOut, "FORMAT") || !strings.Contains(listOut, "physical") {
		return fmt.Errorf("snapshot list does not show the physical format; output: %s", listOut)
	}

	log.Println("Step 5: Writing more rows, then restoring the instance from the snapshot")
	if err := h.execSQLAsUser(port, dbName, "postgres", postgresPassword,
		"INSERT INTO things VALUES (4),(5);"); err != nil {
		return fmt.Errorf("write post-snapshot rows: %w", err)
	}
	output, err = h.runCLI("snapshot", "restore-instance",
		"--instance", instanceName, "--file", liveArchive, "--yes")
	if err != nil {
		return fmt.Errorf("physical restore-instance: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Restored instance") {
		return fmt.Errorf("restore-instance did not report success; output: %s", output)
	}
	if !strings.Contains(output, "physical") {
		return fmt.Errorf("restore-instance did not report the physical format; output: %s", output)
	}
	rows, err := countRows(port, dbName, postgresPassword, "things")
	if err != nil {
		return err
	}
	if rows != 3 {
		return fmt.Errorf("things has %d rows after physical restore, want 3 (post-snapshot writes must be gone)", rows)
	}

	log.Println("Step 6: Cold capture — stop the instance, snapshot again")
	if err := h.execSQLAsUser(port, dbName, "postgres", postgresPassword,
		"INSERT INTO things VALUES (4),(5);"); err != nil {
		return fmt.Errorf("write pre-cold rows: %w", err)
	}
	if output, err = h.runCLI("instance", "stop", instanceName); err != nil {
		return fmt.Errorf("stop instance: %w (output: %s)", err, output)
	}
	if output, err = h.runCLI("snapshot", "make"); err != nil {
		return fmt.Errorf("cold snapshot make: %w (output: %s)", err, output)
	}
	// A stopped instance is a REAL capture in physical mode, not
	// configuration-only — that is one of the format's advantages.
	if strings.Contains(output, "configuration-only") {
		return fmt.Errorf("cold capture was reported configuration-only; output: %s", output)
	}
	if !strings.Contains(output, "captured cold") {
		return fmt.Errorf("cold capture is not visible in the output; output: %s", output)
	}

	coldArchive, err := newestSnapshotArchive(backupDir)
	if err != nil {
		return err
	}
	if coldArchive == liveArchive {
		return fmt.Errorf("cold snapshot did not produce a new archive")
	}
	raw, err = readTarMember(coldArchive, "manifest.json")
	if err != nil {
		return fmt.Errorf("read cold manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("parse cold manifest: %w", err)
	}
	if len(manifest.Instances) != 1 || manifest.Instances[0].CaptureMode != "cold" || !manifest.Instances[0].HasData {
		return fmt.Errorf("cold entry = %+v, want hasData with captureMode cold", manifest.Instances)
	}

	log.Println("Step 7: Restoring from the cold snapshot — must come back verified but STOPPED")
	output, err = h.runCLI("snapshot", "restore-instance",
		"--instance", instanceName, "--file", coldArchive, "--yes")
	if err != nil {
		return fmt.Errorf("cold restore-instance: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "left stopped") {
		return fmt.Errorf("cold restore did not explain the instance is left stopped; output: %s", output)
	}
	statusOut, err := h.getInstanceStatusCLI(instanceName)
	if err != nil {
		return fmt.Errorf("instance status after cold restore: %w (output: %s)", err, statusOut)
	}
	if !strings.Contains(statusOut, "stopped") {
		return fmt.Errorf("instance is not stopped after a cold restore; status: %s", statusOut)
	}

	log.Println("Step 8: Starting it and verifying the cold capture's data")
	if output, err = h.runCLI("instance", "start", instanceName); err != nil {
		return fmt.Errorf("start instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready after cold restore: %w", err)
	}
	rows, err = countRows(port, dbName, postgresPassword, "things")
	if err != nil {
		return err
	}
	if rows != 5 {
		return fmt.Errorf("things has %d rows after cold restore, want 5 (the cold copy included the extra rows)", rows)
	}

	log.Println("Step 9: Cleaning up")
	if output, err = h.runCLI("instance", "destroy", instanceName, "--force"); err != nil {
		return fmt.Errorf("destroy instance: %w (output: %s)", err, output)
	}

	log.Println("=== Physical Snapshot Test PASSED ===")
	return nil
}

// testSnapshotLogicalRoundTrip keeps the LOGICAL restore paths covered end to
// end now that the default is physical: --logical make, restore-instance out of
// it, then a daemon-less apply onto a wiped host. Old archives in the field are
// all logical, so these paths must keep working forever.
func testSnapshotLogicalRoundTrip(h *TestHarness) error {
	log.Println("=== Testing Logical Snapshot Round Trip (make --logical -> restore-instance -> wipe -> apply) ===")

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	const (
		port   = 15502
		dbName = "logdb"
	)
	instanceName := fmt.Sprintf("oddk-danger-funct-log-%d", time.Now().Unix())

	log.Println("Step 1: Creating an instance with data")
	output, err := h.runCLI("create",
		"--name", instanceName, "--version", "17",
		"--port", strconv.Itoa(port), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}
	if output, err = h.createDatabaseCLI(instanceName, dbName); err != nil {
		return fmt.Errorf("create database: %w (output: %s)", err, output)
	}
	postgresPassword, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("read postgres password: %w", err)
	}
	postgresPassword = strings.TrimSpace(postgresPassword)
	if err := h.execSQLAsUser(port, dbName, "postgres", postgresPassword,
		"CREATE TABLE items (id int primary key); INSERT INTO items VALUES (1),(2),(3);"); err != nil {
		return fmt.Errorf("seed data: %w", err)
	}

	log.Println("Step 2: Taking a --logical snapshot and parking it off-host")
	if output, err = h.runCLI("snapshot", "make", "--logical"); err != nil {
		return fmt.Errorf("snapshot make --logical: %w (output: %s)", err, output)
	}
	backupDir := filepath.Join(h.dataDir, "backups")
	archivePath, err := newestSnapshotArchive(backupDir)
	if err != nil {
		return err
	}

	offHost, err := os.MkdirTemp("", "oddk-logrt-offhost-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(offHost) }()
	archiveCopy := filepath.Join(offHost, "snapshot.tar.zst")
	keyCopy := filepath.Join(offHost, "master.key")
	if err := copyFileForTest(archivePath, archiveCopy); err != nil {
		return fmt.Errorf("copy archive off host: %w", err)
	}
	if err := copyFileForTest(filepath.Join(h.dataDir, "master.key"), keyCopy); err != nil {
		return fmt.Errorf("copy master key off host: %w", err)
	}

	log.Println("Step 3: Logical restore-instance reverts post-snapshot writes")
	if err := h.execSQLAsUser(port, dbName, "postgres", postgresPassword,
		"INSERT INTO items VALUES (4),(5);"); err != nil {
		return fmt.Errorf("write post-snapshot rows: %w", err)
	}
	output, err = h.runCLI("snapshot", "restore-instance",
		"--instance", instanceName, "--file", archivePath, "--yes")
	if err != nil {
		return fmt.Errorf("logical restore-instance: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Restored instance") {
		return fmt.Errorf("restore-instance did not report success; output: %s", output)
	}
	rows, err := countRows(port, dbName, postgresPassword, "items")
	if err != nil {
		return err
	}
	if rows != 3 {
		return fmt.Errorf("items has %d rows after logical restore, want 3", rows)
	}

	log.Println("Step 4: Wiping the host and applying the logical snapshot daemon-less")
	if output, err = h.runCLI("instance", "destroy", instanceName, "--force"); err != nil {
		return fmt.Errorf("destroy instance: %w (output: %s)", err, output)
	}
	if err := h.stopDaemon(); err != nil {
		return fmt.Errorf("stop daemon: %w", err)
	}
	for _, name := range []string{"oddk.db", "oddk.db-wal", "oddk.db-shm", "master.key"} {
		if err := os.Remove(filepath.Join(h.dataDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("wipe %s: %w", name, err)
		}
	}

	output, err = h.runCLI("snapshot", "apply",
		"--file", archiveCopy,
		"--master-key", keyCopy,
		"--data-dir", h.dataDir,
		"--daemon-port", strconv.Itoa(testPort),
		"--yes")
	if err != nil {
		return fmt.Errorf("logical snapshot apply: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Snapshot applied") {
		return fmt.Errorf("apply did not report success; output: %s", output)
	}

	log.Println("Step 5: Verifying the deployment came back")
	if err := h.startDaemon(); err != nil {
		return fmt.Errorf("start daemon after apply: %w", err)
	}
	statusOut, err := h.getInstanceStatusCLI(instanceName)
	if err != nil {
		return fmt.Errorf("instance status after apply: %w (output: %s)", err, statusOut)
	}
	if !strings.Contains(statusOut, "running") {
		return fmt.Errorf("instance is not running after apply; status: %s", statusOut)
	}
	rows, err = countRows(port, dbName, postgresPassword, "items")
	if err != nil {
		return err
	}
	if rows != 3 {
		return fmt.Errorf("items has %d rows after apply, want 3", rows)
	}

	log.Println("Step 6: Cleaning up")
	if output, err = h.runCLI("instance", "destroy", instanceName, "--force"); err != nil {
		return fmt.Errorf("destroy instance: %w (output: %s)", err, output)
	}

	log.Println("=== Logical Snapshot Round Trip Test PASSED ===")
	return nil
}

// newestSnapshotArchive returns the most recent snapshot archive in dir.
// Snapshot filenames embed a sortable UTC timestamp, so lexical order is
// chronological.
func newestSnapshotArchive(dir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "snapshot-*.tar.zst"))
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf("no snapshot archive found in %s (err %v)", dir, err)
	}
	sort.Strings(matches)
	return matches[len(matches)-1], nil
}

// countRows counts the rows of one table over a fresh connection.
func countRows(port int, dbName, postgresPassword, table string) (int, error) {
	conn, err := pgConnect(port, "postgres", postgresPassword, dbName)
	if err != nil {
		return 0, fmt.Errorf("connect to count %s: %w", table, err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var n int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return n, nil
}
