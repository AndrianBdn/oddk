package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// testSnapshotRestoreInstance covers restoring ONE instance out of a snapshot
// into a deployment that stays up — the "this instance is corrupted, put it
// back" path, as opposed to whole-host DR.
//
// Three things are asserted that nothing else covers:
//   - post-snapshot writes are GONE (the cluster is rebuilt, not merged into)
//   - a SECOND instance is completely untouched, including still serving reads
//     while the first is torn down and rebuilt
//   - the instance can be restored when it does not exist at all, recreating it
//     from the archive's instance.json
func testSnapshotRestoreInstance(h *TestHarness) error {
	log.Println("=== Testing Snapshot Restore Single Instance ===")

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	const (
		portA    = 15491
		portB    = 15492
		dbName   = "ridb"
		ownerUsr = "riowner"
	)
	stamp := time.Now().Unix()
	targetName := fmt.Sprintf("oddk-danger-funct-ri-a-%d", stamp)
	bystanderName := fmt.Sprintf("oddk-danger-funct-ri-b-%d", stamp)

	log.Println("Step 1: Two instances, one with owned data")
	output, err := h.runCLI("create",
		"--name", targetName, "--version", "17",
		"--port", strconv.Itoa(portA), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create target instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(portA); err != nil {
		return fmt.Errorf("target PostgreSQL not ready: %w", err)
	}
	defer func() { _, _ = h.runCLI("instance", "destroy", targetName, "--force") }()

	output, err = h.runCLI("create",
		"--name", bystanderName, "--version", "17",
		"--port", strconv.Itoa(portB), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create bystander instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(portB); err != nil {
		return fmt.Errorf("bystander PostgreSQL not ready: %w", err)
	}
	defer func() { _, _ = h.runCLI("instance", "destroy", bystanderName, "--force") }()

	if output, err = h.createDatabaseCLI(targetName, dbName); err != nil {
		return fmt.Errorf("create database: %w (output: %s)", err, output)
	}
	output, err = h.runCLI("instance", "add-db-user", targetName,
		"--username="+ownerUsr, "--database="+dbName, "--owner")
	if err != nil {
		return fmt.Errorf("add owner user: %w (output: %s)", err, output)
	}
	ownerPassword, err := extractCredentialPassword(output)
	if err != nil {
		return fmt.Errorf("extract owner password: %w", err)
	}
	if err := h.execSQLAsUser(portA, dbName, ownerUsr, ownerPassword,
		"CREATE TABLE widgets (id int primary key, label text); "+
			"INSERT INTO widgets VALUES (1,'alpha'),(2,'beta'),(3,'gamma');"); err != nil {
		return fmt.Errorf("seed data as owner: %w", err)
	}

	// Give the bystander its own data so "untouched" is a real assertion.
	if output, err = h.createDatabaseCLI(bystanderName, "bystander_db"); err != nil {
		return fmt.Errorf("create bystander database: %w (output: %s)", err, output)
	}

	targetPassword, err := h.getPasswordCLI(targetName, "--plain")
	if err != nil {
		return fmt.Errorf("read target postgres password: %w", err)
	}
	targetPassword = strings.TrimSpace(targetPassword)
	bystanderPassword, err := h.getPasswordCLI(bystanderName, "--plain")
	if err != nil {
		return fmt.Errorf("read bystander postgres password: %w", err)
	}
	bystanderPassword = strings.TrimSpace(bystanderPassword)

	log.Println("Step 2: Snapshot, then write MORE data that must not survive the restore")
	if output, err = h.runCLI("snapshot", "make"); err != nil {
		return fmt.Errorf("snapshot make: %w (output: %s)", err, output)
	}
	matches, err := filepath.Glob(filepath.Join(h.dataDir, "backups", "snapshot-*.tar.zst"))
	if err != nil || len(matches) != 1 {
		return fmt.Errorf("expected 1 snapshot archive, found %v (err %v)", matches, err)
	}
	archivePath := matches[0]

	if err := h.execSQLAsUser(portA, dbName, ownerUsr, ownerPassword,
		"INSERT INTO widgets VALUES (4,'post-snapshot'),(5,'also-post');"); err != nil {
		return fmt.Errorf("write post-snapshot rows: %w", err)
	}

	log.Println("Step 3: Restoring the target instance over itself")
	output, err = h.runCLI("snapshot", "restore-instance",
		"--instance", targetName, "--file", archivePath, "--yes")
	if err != nil {
		return fmt.Errorf("restore-instance: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Restored instance") {
		return fmt.Errorf("restore-instance did not report a restore; output: %s", output)
	}

	if err := h.waitForPostgreSQL(portA); err != nil {
		return fmt.Errorf("target not ready after restore: %w", err)
	}

	adminConn, err := pgConnect(portA, "postgres", targetPassword, dbName)
	if err != nil {
		return fmt.Errorf("postgres cannot connect after restore: %w", err)
	}
	defer func() { _ = adminConn.Close(context.Background()) }()

	// The decisive assertion: the cluster was REBUILT, so rows written after the
	// snapshot are gone. A merge-style restore would leave 5.
	var rows int
	if err := adminConn.QueryRow(context.Background(), "SELECT count(*) FROM widgets").Scan(&rows); err != nil {
		return fmt.Errorf("query restored table: %w", err)
	}
	if rows != 3 {
		return fmt.Errorf("widgets has %d rows after restore, want 3 (post-snapshot writes must not survive)", rows)
	}

	// Roles and ownership must come back, exactly as for a whole-host apply.
	if _, err := pgConnect(portA, ownerUsr, ownerPassword, dbName); err != nil {
		return fmt.Errorf("owner role cannot log in with its original password after restore: %w", err)
	}
	var tableOwner string
	if err := adminConn.QueryRow(context.Background(),
		"SELECT tableowner FROM pg_tables WHERE tablename = 'widgets'").Scan(&tableOwner); err != nil {
		return fmt.Errorf("read table owner: %w", err)
	}
	if tableOwner != ownerUsr {
		return fmt.Errorf("widgets is owned by %q after restore, want %q", tableOwner, ownerUsr)
	}

	log.Println("Step 4: Verifying the bystander instance was untouched")
	statusOut, err := h.getInstanceStatusCLI(bystanderName)
	if err != nil {
		return fmt.Errorf("bystander status: %w (output: %s)", err, statusOut)
	}
	if !strings.Contains(statusOut, "running") {
		return fmt.Errorf("bystander is not running after the target was restored; status: %s", statusOut)
	}
	stillThere, err := h.getPasswordCLI(bystanderName, "--plain")
	if err != nil {
		return fmt.Errorf("read bystander password after restore: %w", err)
	}
	if strings.TrimSpace(stillThere) != bystanderPassword {
		return fmt.Errorf("bystander postgres password changed during another instance's restore")
	}
	bystanderConn, err := pgConnect(portB, "postgres", bystanderPassword, "bystander_db")
	if err != nil {
		return fmt.Errorf("bystander cannot serve connections after the target was restored: %w", err)
	}
	_ = bystanderConn.Close(context.Background())

	log.Println("Step 5: Destroying the target entirely, then restoring it back into existence")
	if output, err = h.runCLI("instance", "destroy", targetName, "--force"); err != nil {
		return fmt.Errorf("destroy target: %w (output: %s)", err, output)
	}

	output, err = h.runCLI("snapshot", "restore-instance",
		"--instance", targetName, "--file", archivePath, "--yes")
	if err != nil {
		return fmt.Errorf("restore-instance onto a destroyed instance: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Created and restored instance") {
		return fmt.Errorf("restore did not report creating the instance; output: %s", output)
	}
	if err := h.waitForPostgreSQL(portA); err != nil {
		return fmt.Errorf("recreated target not ready: %w", err)
	}

	// A recreated instance must carry the SNAPSHOT's postgres password, because
	// globals.sql restores the source's hash and only that plaintext matches it.
	recreatedPassword, err := h.getPasswordCLI(targetName, "--plain")
	if err != nil {
		return fmt.Errorf("read recreated postgres password: %w", err)
	}
	if strings.TrimSpace(recreatedPassword) != targetPassword {
		return fmt.Errorf("recreated instance does not carry the snapshot's postgres password")
	}
	recreatedConn, err := pgConnect(portA, "postgres", targetPassword, dbName)
	if err != nil {
		return fmt.Errorf("postgres cannot connect to the recreated instance: %w", err)
	}
	defer func() { _ = recreatedConn.Close(context.Background()) }()

	if err := recreatedConn.QueryRow(context.Background(), "SELECT count(*) FROM widgets").Scan(&rows); err != nil {
		return fmt.Errorf("query table on recreated instance: %w", err)
	}
	if rows != 3 {
		return fmt.Errorf("widgets has %d rows on the recreated instance, want 3", rows)
	}

	log.Println("Step 6: Refusals")
	// An instance that is not in the archive must be refused by name, not
	// discovered halfway through a rebuild.
	output, err = h.runCLI("snapshot", "restore-instance",
		"--instance", "oddk-danger-funct-nonexistent", "--file", archivePath, "--yes")
	if err == nil {
		return fmt.Errorf("restoring an instance absent from the snapshot should fail; output: %s", output)
	}
	// runCLI returns the daemon's message in err, not stdout.
	if !strings.Contains(err.Error(), "does not contain instance") {
		return fmt.Errorf("expected a 'not in snapshot' refusal, got: %v (output: %s)", err, output)
	}

	log.Println("=== Snapshot Restore Single Instance Test PASSED ===")
	return nil
}
