//go:build oddk_debug

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// testSnapshotRestoreDatabase covers restoring ONE database out of a snapshot
// into a live instance, as a new database — from both archive formats.
//
// What it asserts that nothing else covers:
//   - a PHYSICAL snapshot (no per-database dumps) yields the database's
//     snapshot-time contents, via a scratch copy of the cluster;
//   - the live database, the instance's other databases and its postgres
//     password are untouched — the whole point of choosing this over
//     restore-instance;
//   - no scratch container and no scratch volume outlive the restore;
//   - --from-instance: another instance's database into a target that is not
//     in the snapshot at all;
//   - the refusals: an existing target, a database the snapshot does not hold,
//     an instance that does not exist.
func testSnapshotRestoreDatabase(h *TestHarness) error {
	log.Println("=== Testing Snapshot Restore Single Database ===")

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	const (
		port   = 15511
		portB  = 15512
		dbName = "rdbsales"
		other  = "rdbother"
	)
	instanceName := fmt.Sprintf("oddk-danger-funct-rdb-%d", time.Now().Unix())

	log.Println("Step 1: An instance with two databases and data")
	output, err := h.runCLI("create",
		"--name", instanceName, "--version", "17",
		"--port", strconv.Itoa(port), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}
	defer func() { _, _ = h.runCLI("instance", "destroy", instanceName, "--force") }()

	for _, db := range []string{dbName, other} {
		if output, err = h.createDatabaseCLI(instanceName, db); err != nil {
			return fmt.Errorf("create database %s: %w (output: %s)", db, err, output)
		}
	}
	password, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("get password: %w", err)
	}
	password = strings.TrimSpace(password)
	if err := h.execSQLAsUser(port, dbName, "postgres", password,
		"CREATE TABLE orders (id int primary key, note text); "+
			"INSERT INTO orders SELECT g, 'snapshot-time' FROM generate_series(1, 250) g;"); err != nil {
		return fmt.Errorf("seed %s: %w", dbName, err)
	}
	if err := h.execSQLAsUser(port, other, "postgres", password,
		"CREATE TABLE keep (id int); INSERT INTO keep VALUES (1),(2);"); err != nil {
		return fmt.Errorf("seed %s: %w", other, err)
	}

	log.Println("Step 2: A physical snapshot, then the live database diverges")
	if output, err = h.runCLI("snapshot", "make"); err != nil {
		return fmt.Errorf("snapshot make: %w (output: %s)", err, output)
	}
	records, err := listSnapshotRecords(h)
	if err != nil {
		return err
	}
	physicalID := strconv.Itoa(records[0].ID)
	// Someone "drops a table": the reason this command exists.
	if err := h.execSQLAsUser(port, dbName, "postgres", password,
		"DELETE FROM orders WHERE id > 10; INSERT INTO orders VALUES (9001, 'after-snapshot');"); err != nil {
		return fmt.Errorf("diverge %s: %w", dbName, err)
	}
	if err := h.execSQLAsUser(port, other, "postgres", password, "INSERT INTO keep VALUES (3);"); err != nil {
		return fmt.Errorf("diverge %s: %w", other, err)
	}

	log.Println("Step 3: Restoring over the existing database is refused")
	output, err = h.runCLI("snapshot", "restore-database", "--instance", instanceName,
		"--database", dbName, "--id", physicalID, "--yes")
	if err == nil {
		return fmt.Errorf("restore over an existing database was accepted; output: %s", output)
	}
	if !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "--restore-as") {
		return fmt.Errorf("expected an 'already exists ... --restore-as' refusal, got: %v", err)
	}

	log.Println("Step 4: Physical restore under a new name")
	output, err = h.runCLI("snapshot", "restore-database", "--instance", instanceName,
		"--database", dbName, "--restore-as", "rdbsales_physical", "--id", physicalID, "--yes")
	if err != nil {
		return fmt.Errorf("physical restore-database: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Restored database rdbsales_physical") {
		return fmt.Errorf("unexpected output: %s", output)
	}
	if err := expectOrders(port, "rdbsales_physical", password, 250, 0); err != nil {
		return fmt.Errorf("physical restore: %w", err)
	}

	log.Println("Step 5: Nothing else changed")
	// The live database keeps its post-snapshot state, the other database its
	// post-snapshot row, and the stored password still authenticates.
	if err := expectOrders(port, dbName, password, 11, 1); err != nil {
		return fmt.Errorf("live database was touched: %w", err)
	}
	if n, err := countRows(port, other, password, "keep"); err != nil || n != 3 {
		return fmt.Errorf("other database was touched: %d rows, %v (want 3)", n, err)
	}

	log.Println("Step 6: No scratch cluster or scratch volume survives")
	if err := expectNoScratchLeftovers(); err != nil {
		return err
	}

	log.Println("Step 7: A database the snapshot does not hold is refused, naming what it has")
	output, err = h.runCLI("snapshot", "restore-database", "--instance", instanceName,
		"--database", "nosuchdb", "--id", physicalID, "--yes")
	if err == nil {
		return fmt.Errorf("restoring a missing database was accepted; output: %s", output)
	}
	if !strings.Contains(err.Error(), "not found in snapshot") || !strings.Contains(err.Error(), dbName) {
		return fmt.Errorf("expected 'not found in snapshot' listing %s, got: %v", dbName, err)
	}
	if err := expectNoScratchLeftovers(); err != nil {
		return fmt.Errorf("after a refused restore: %w", err)
	}

	log.Println("Step 8: A logical snapshot restores the same way")
	if err := h.execSQLAsUser(port, dbName, "postgres", password,
		"INSERT INTO orders VALUES (9002, 'in-logical');"); err != nil {
		return fmt.Errorf("mark logical state: %w", err)
	}
	if output, err = h.runCLI("snapshot", "make", "--logical"); err != nil {
		return fmt.Errorf("snapshot make --logical: %w (output: %s)", err, output)
	}
	records, err = listSnapshotRecords(h)
	if err != nil {
		return err
	}
	output, err = h.runCLI("snapshot", "restore-database", "--instance", instanceName,
		"--database", dbName, "--restore-as", "rdbsales_logical", "--id", strconv.Itoa(records[0].ID), "--yes")
	if err != nil {
		return fmt.Errorf("logical restore-database: %w (output: %s)", err, output)
	}
	if err := expectOrders(port, "rdbsales_logical", password, 12, 2); err != nil {
		return fmt.Errorf("logical restore: %w", err)
	}

	log.Println("Step 8b: A name that looks like a connection string is just a name")
	// libpq reads a --dbname containing "=" as a conninfo string. Passed bare,
	// this restored into the EXISTING postgres database after the "target does
	// not exist" check had passed on the literal name.
	output, err = h.runCLI("snapshot", "restore-database", "--instance", instanceName,
		"--database", dbName, "--restore-as", "dbname=postgres", "--id", physicalID, "--yes")
	if err != nil {
		return fmt.Errorf("restore-as a conninfo-shaped name: %w (output: %s)", err, output)
	}
	if err := expectOrders(port, "dbname=postgres", password, 250, 0); err != nil {
		return fmt.Errorf("the literal database did not receive the data: %w", err)
	}
	if _, err := countRows(port, "postgres", password, "orders"); err == nil {
		return fmt.Errorf("the restore wrote into the existing postgres database")
	}

	log.Println("Step 8c: A catalogued local copy replaced by another valid archive is refused")
	// The frame check cannot see this — the substitute is intact. Only the
	// digest recorded when the snapshot was written can.
	records, err = listSnapshotRecords(h)
	if err != nil {
		return err
	}
	var physicalPath, logicalPath string
	for _, r := range records {
		switch strconv.Itoa(r.ID) {
		case physicalID:
			physicalPath = r.LocalLocation
		default:
			if logicalPath == "" {
				logicalPath = r.LocalLocation
			}
		}
	}
	if physicalPath == "" || logicalPath == "" {
		return fmt.Errorf("need two local snapshots, got %+v", records)
	}
	original, err := os.ReadFile(physicalPath)
	if err != nil {
		return err
	}
	substitute, err := os.ReadFile(logicalPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(physicalPath, substitute, 0o600); err != nil {
		return err
	}
	output, err = h.runCLI("snapshot", "restore-database", "--instance", instanceName,
		"--database", dbName, "--restore-as", "rdbsales_substituted", "--id", physicalID, "--yes")
	if restoreErr := os.WriteFile(physicalPath, original, 0o600); restoreErr != nil {
		return restoreErr
	}
	if err == nil {
		return fmt.Errorf("a replaced catalogued archive was restored; output: %s", output)
	}
	if !strings.Contains(err.Error(), "NOT the archive that was catalogued") {
		return fmt.Errorf("expected a digest-mismatch refusal, got: %v", err)
	}

	log.Println("Step 9: --from-instance restores another instance's database (prod -> staging)")
	// The target was created AFTER the snapshot, so it is not in it at all:
	// only the source entry has to be.
	stagingName := instanceName + "-stg"
	output, err = h.runCLI("create",
		"--name", stagingName, "--version", "17",
		"--port", strconv.Itoa(portB), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create staging instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(portB); err != nil {
		return fmt.Errorf("staging PostgreSQL not ready: %w", err)
	}
	defer func() { _, _ = h.runCLI("instance", "destroy", stagingName, "--force") }()
	stagingPassword, err := h.getPasswordCLI(stagingName, "--plain")
	if err != nil {
		return fmt.Errorf("get staging password: %w", err)
	}
	stagingPassword = strings.TrimSpace(stagingPassword)

	output, err = h.runCLI("snapshot", "restore-database", "--instance", stagingName,
		"--from-instance", instanceName, "--database", dbName, "--id", physicalID, "--yes")
	if err != nil {
		return fmt.Errorf("cross-instance restore-database: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "of instance "+instanceName) {
		return fmt.Errorf("output does not name the source instance: %s", output)
	}
	if err := expectOrders(portB, dbName, stagingPassword, 250, 0); err != nil {
		return fmt.Errorf("cross-instance restore: %w", err)
	}
	// The source is untouched by a restore that only READ its snapshot copy.
	if err := expectOrders(port, dbName, password, 12, 2); err != nil {
		return fmt.Errorf("source instance was touched: %w", err)
	}
	if err := expectNoScratchLeftovers(); err != nil {
		return err
	}

	log.Println("Step 10: An instance that does not exist is refused, pointing at restore-instance")
	output, err = h.runCLI("snapshot", "restore-database", "--instance", "oddk-danger-funct-rdb-missing",
		"--database", dbName, "--id", physicalID, "--yes")
	if err == nil {
		return fmt.Errorf("restore into a missing instance was accepted; output: %s", output)
	}
	if !strings.Contains(err.Error(), "restore-instance") {
		return fmt.Errorf("refusal does not point at restore-instance: %v", err)
	}

	log.Println("Step 11: An orphaned scratch volume is removed at daemon startup")
	// What a crash, or a volume delete Docker reported as done but was not,
	// leaves behind. Nothing references it, so only its label can find it.
	orphan := fmt.Sprintf("oddk-scratch-orphan-%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "volume", "create", "--label", "oddk.scratch-volume=e2e", orphan).CombinedOutput(); err != nil {
		return fmt.Errorf("create orphan volume: %w (%s)", err, out)
	}
	defer func() { _ = exec.Command("docker", "volume", "rm", "-f", orphan).Run() }()
	if err := h.restartDaemon(); err != nil {
		return fmt.Errorf("restart daemon: %w", err)
	}
	if err := expectNoScratchLeftovers(); err != nil {
		return fmt.Errorf("after a daemon restart: %w", err)
	}

	log.Println("=== Snapshot Restore Single Database Test PASSED ===")
	return nil
}

// expectOrders checks the orders table's row count and how many rows carry a
// post-snapshot note.
func expectOrders(port int, db, password string, wantRows, wantAfter int) error {
	conn, err := pgConnect(port, "postgres", password, db)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", db, err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var rows, after int
	if err := conn.QueryRow(context.Background(),
		"SELECT count(*), count(*) FILTER (WHERE note <> 'snapshot-time') FROM orders").Scan(&rows, &after); err != nil {
		return fmt.Errorf("count orders in %s: %w", db, err)
	}
	if rows != wantRows || after != wantAfter {
		return fmt.Errorf("%s has %d orders (%d post-snapshot), want %d (%d)", db, rows, after, wantRows, wantAfter)
	}
	return nil
}

// expectNoScratchLeftovers asserts no scratch container AND no scratch volume
// survives. The volume is checked on its own: Docker can remove a container and
// fail to delete its volume while reporting success, and that volume is a full
// copy of an instance's data.
func expectNoScratchLeftovers() error {
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=oddk.scratch-copy").CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker ps: %w (%s)", err, out)
	}
	if ids := strings.TrimSpace(string(out)); ids != "" {
		return fmt.Errorf("scratch container(s) left behind: %s", ids)
	}
	out, err = exec.Command("docker", "volume", "ls", "-q", "--filter", "label=oddk.scratch-volume").CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker volume ls: %w (%s)", err, out)
	}
	if names := strings.TrimSpace(string(out)); names != "" {
		return fmt.Errorf("scratch volume(s) left behind: %s", names)
	}
	return nil
}
