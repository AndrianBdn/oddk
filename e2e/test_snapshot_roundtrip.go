package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/image"
)

// testSnapshotRoundTrip is the test the whole snapshot feature exists for:
// capture a deployment, wipe the host down to a fresh-install state, apply the
// snapshot, and prove the result is the deployment that was captured.
//
// The assertions that matter are the ones a per-database 'backup restore'
// cannot satisfy — roles come back with their ORIGINAL passwords, and object
// ownership survives instead of being flattened to postgres. Those were the
// gaps that made host migration impossible before.
func testSnapshotRoundTrip(h *TestHarness) error {
	log.Println("=== Testing Snapshot Round Trip (capture -> wipe host -> apply) ===")

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	const (
		port     = 15481
		dbName   = "rtdb"
		ownerUsr = "rtowner"
	)
	instanceName := fmt.Sprintf("oddk-danger-funct-rt-%d", time.Now().Unix())

	log.Println("Step 1: Building a deployment with data, an owner role, and owned objects")
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

	// --owner so the role owns the database and its objects; that ownership is
	// precisely what a plain restore loses.
	output, err = h.runCLI("instance", "add-db-user", instanceName,
		"--username="+ownerUsr, "--database="+dbName, "--owner")
	if err != nil {
		return fmt.Errorf("add owner user: %w (output: %s)", err, output)
	}
	ownerPassword, err := extractCredentialPassword(output)
	if err != nil {
		return fmt.Errorf("extract owner password: %w", err)
	}

	// Create the table AS the owner so it is genuinely owner-owned.
	if err := h.execSQLAsUser(port, dbName, ownerUsr, ownerPassword,
		"CREATE TABLE widgets (id int primary key, label text); "+
			"INSERT INTO widgets VALUES (1,'alpha'),(2,'beta'),(3,'gamma');"); err != nil {
		return fmt.Errorf("seed data as owner: %w", err)
	}

	postgresPassword, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("read postgres password: %w", err)
	}
	postgresPassword = strings.TrimSpace(postgresPassword)

	log.Println("Step 2: Taking the snapshot")
	if output, err = h.runCLI("snapshot", "make"); err != nil {
		return fmt.Errorf("snapshot make: %w (output: %s)", err, output)
	}
	backupDir := filepath.Join(h.dataDir, "backups")
	matches, err := filepath.Glob(filepath.Join(backupDir, "snapshot-*.tar.zst"))
	if err != nil || len(matches) != 1 {
		return fmt.Errorf("expected 1 snapshot archive, found %v (err %v)", matches, err)
	}

	// Park the archive and key outside the data dir - on a real migration they
	// arrive from elsewhere, and the host is about to be wiped.
	offHost, err := os.MkdirTemp("", "oddk-rt-offhost-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(offHost) }()

	archiveCopy := filepath.Join(offHost, "snapshot.tar.zst")
	keyCopy := filepath.Join(offHost, "master.key")
	if err := copyFileForTest(matches[0], archiveCopy); err != nil {
		return fmt.Errorf("copy archive off host: %w", err)
	}
	if err := copyFileForTest(filepath.Join(h.dataDir, "master.key"), keyCopy); err != nil {
		return fmt.Errorf("copy master key off host: %w", err)
	}

	log.Println("Step 3: Wiping the host (destroy instance, then reset to fresh-install state)")
	// Destroying removes the container AND the data volume, so nothing of the
	// instance survives - as on a genuinely new machine.
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
	// Bring a daemon up once and straight back down: this reproduces the state
	// the installer actually leaves behind - a generated master.key and an
	// initialised, empty oddk.db - which is the collision 'apply' must resolve
	// on its own rather than asking the operator to delete a key.
	if err := h.startDaemon(); err != nil {
		return fmt.Errorf("simulate fresh install: %w", err)
	}
	if err := h.stopDaemon(); err != nil {
		return fmt.Errorf("stop freshly installed daemon: %w", err)
	}
	freshKey, err := os.ReadFile(filepath.Join(h.dataDir, "master.key"))
	if err != nil {
		return fmt.Errorf("fresh install did not generate a master.key: %w", err)
	}

	// Remove the image too. A genuinely new host has an empty image cache —
	// oddk-install.sh never pulls one — and without this the test cannot see
	// whether apply handles a missing image before or after the point of no
	// return. This is the step whose absence hid exactly that bug.
	log.Println("Step 3b: Removing the PostgreSQL image to simulate an empty image cache")
	if _, err := h.docker.ImageRemove(context.Background(), "postgres:17",
		image.RemoveOptions{Force: true, PruneChildren: false}); err != nil {
		return fmt.Errorf("remove image to simulate a fresh host: %w", err)
	}

	log.Println("Step 4: Applying the snapshot with the daemon stopped")
	output, err = h.runCLI("snapshot", "apply",
		"--file", archiveCopy,
		"--master-key", keyCopy,
		"--data-dir", h.dataDir,
		"--daemon-port", strconv.Itoa(testPort),
		"--yes")
	if err != nil {
		return fmt.Errorf("snapshot apply: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Snapshot applied") {
		return fmt.Errorf("apply did not report success; output: %s", output)
	}
	if !strings.Contains(output, "previous key saved as") {
		return fmt.Errorf("apply did not report replacing the generated master.key; output: %s", output)
	}
	// Apply must have pulled the image it needs during preflight, BEFORE
	// replacing oddk.db and master.key.
	if !strings.Contains(output, "Instance images are available") {
		return fmt.Errorf("apply did not report an image preflight check; output: %s", output)
	}
	if _, exists := h.imageExists("postgres:17"); !exists {
		return fmt.Errorf("apply did not pull postgres:17 back onto the host")
	}

	// The generated key must have been displaced by the snapshot's.
	installedKey, err := os.ReadFile(filepath.Join(h.dataDir, "master.key"))
	if err != nil {
		return fmt.Errorf("read installed master.key: %w", err)
	}
	if string(installedKey) == string(freshKey) {
		return fmt.Errorf("master.key was not replaced by the snapshot's key")
	}
	replaced, _ := filepath.Glob(filepath.Join(h.dataDir, "master.key.replaced-*"))
	if len(replaced) != 1 {
		return fmt.Errorf("expected the displaced key to be preserved, found %v", replaced)
	}

	log.Println("Step 5: Starting the daemon and verifying the deployment came back")
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

	// The postgres password must still be the one ODDK has stored - proving
	// globals.sql's ALTER ROLE postgres was a no-op rather than locking us out.
	restoredPgPassword, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("read postgres password after apply: %w", err)
	}
	if strings.TrimSpace(restoredPgPassword) != postgresPassword {
		return fmt.Errorf("postgres password changed across the restore")
	}
	adminConn, err := pgConnect(port, "postgres", postgresPassword, dbName)
	if err != nil {
		return fmt.Errorf("postgres cannot connect after apply: %w", err)
	}
	defer func() { _ = adminConn.Close(context.Background()) }()

	log.Println("Step 6: Verifying data, role passwords and object ownership")
	var rows int
	if err := adminConn.QueryRow(context.Background(), "SELECT count(*) FROM widgets").Scan(&rows); err != nil {
		return fmt.Errorf("query restored table: %w", err)
	}
	if rows != 3 {
		return fmt.Errorf("widgets has %d rows after restore, want 3", rows)
	}

	// The owner role must authenticate with its ORIGINAL password: the hash
	// came back via globals.sql. A per-database 'backup restore' cannot do this.
	ownerConn, err := pgConnect(port, ownerUsr, ownerPassword, dbName)
	if err != nil {
		return fmt.Errorf("owner role cannot log in with its original password after restore: %w", err)
	}
	defer func() { _ = ownerConn.Close(context.Background()) }()

	// Ownership must survive rather than being flattened to postgres.
	var tableOwner string
	if err := adminConn.QueryRow(context.Background(),
		"SELECT tableowner FROM pg_tables WHERE tablename = 'widgets'").Scan(&tableOwner); err != nil {
		return fmt.Errorf("read table owner: %w", err)
	}
	if tableOwner != ownerUsr {
		return fmt.Errorf("widgets is owned by %q after restore, want %q - ownership was not preserved", tableOwner, ownerUsr)
	}

	var dbOwner string
	if err := adminConn.QueryRow(context.Background(),
		"SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1", dbName).Scan(&dbOwner); err != nil {
		return fmt.Errorf("read database owner: %w", err)
	}
	if dbOwner != ownerUsr {
		return fmt.Errorf("database %s is owned by %q after restore, want %q", dbName, dbOwner, ownerUsr)
	}

	// The owner must still be able to write - grants, not just ownership rows.
	if err := h.execSQLAsUser(port, dbName, ownerUsr, ownerPassword,
		"INSERT INTO widgets VALUES (4,'delta');"); err != nil {
		return fmt.Errorf("owner cannot write after restore: %w", err)
	}

	log.Println("Step 7: Cleaning up")
	if output, err = h.runCLI("instance", "destroy", instanceName, "--force"); err != nil {
		return fmt.Errorf("destroy instance: %w (output: %s)", err, output)
	}

	log.Println("=== Snapshot Round Trip Test PASSED ===")
	return nil
}

func copyFileForTest(src, dst string) error {
	data, err := os.ReadFile(src) // #nosec G304 - test-controlled path
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}
