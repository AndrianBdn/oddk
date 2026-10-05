//go:build oddk_debug

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgCronImage is postgres:17 plus pg_cron, built locally on first use from the
// PGDG apt repository the official image already configures.
const pgCronImage = "oddk-e2e-pgcron:17"

// testSnapshotRestoreDatabaseScratchJobs proves the scratch copy of a cluster
// cannot run the source's scheduled jobs before the database is dumped out of
// it.
//
// The copy starts with the instance's own parameter group, so a preloaded
// pg_cron comes up with it and runs every job the snapshot recorded — with no
// network needed, since a job that deletes local rows only touches the copy.
// The restore would then "succeed" with data the snapshot never held.
//
// The job here deletes every row once a deadline passes. The deadline is after
// the snapshot and before the restore, so the live instance loses its rows
// (the control: the job really runs) while the restored copy must keep all of
// them.
func testSnapshotRestoreDatabaseScratchJobs(h *TestHarness) error {
	log.Println("=== Testing Snapshot Restore Database: scratch cluster runs no jobs ===")

	if err := ensurePgCronImage(); err != nil {
		return err
	}

	const (
		port   = 15513
		dbName = "cronsales"
		group  = "e2e-pgcron"
		rows   = 100
	)
	instanceName := fmt.Sprintf("oddk-danger-funct-cron-%d", time.Now().Unix())

	log.Println("Step 1: A parameter group that preloads pg_cron")
	params := []map[string]any{
		{"name": "shared_preload_libraries", "type": "postgres_cli_arg", "valueType": "string", "value": "pg_cron"},
		{"name": "cron.database_name", "type": "postgres_cli_arg", "valueType": "string", "value": "postgres"},
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return err
	}
	paramsFile := filepath.Join(h.dataDir, "pgcron-params.json")
	if err := os.WriteFile(paramsFile, paramsJSON, 0o600); err != nil {
		return err
	}
	if output, err := h.createParameterGroupCLI(group, paramsFile); err != nil {
		return fmt.Errorf("create parameter group: %w (output: %s)", err, output)
	}

	log.Println("Step 2: An instance running pg_cron, with a job that wipes a table after a deadline")
	output, err := h.runCLI("create",
		"--name", instanceName, "--version", "17", "--image", pgCronImage,
		"--parameter-group", group,
		"--port", strconv.Itoa(port), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}
	defer func() { _, _ = h.runCLI("instance", "destroy", instanceName, "--force") }()

	if output, err = h.createDatabaseCLI(instanceName, dbName); err != nil {
		return fmt.Errorf("create database: %w (output: %s)", err, output)
	}
	password, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return err
	}
	password = strings.TrimSpace(password)
	if err := h.execSQLAsUser(port, dbName, "postgres", password, fmt.Sprintf(
		"CREATE TABLE orders (id int primary key, note text); "+
			"INSERT INTO orders SELECT g, 'snapshot-time' FROM generate_series(1, %d) g;", rows)); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	// The deadline leaves room for the snapshot, and is well before the
	// restore. pg_cron >= 1.5 accepts second-granularity schedules.
	deadline := time.Now().UTC().Add(25 * time.Second).Format("2006-01-02 15:04:05")
	if err := h.execSQLAsUser(port, "postgres", "postgres", password, fmt.Sprintf(
		"CREATE EXTENSION pg_cron; "+
			"SELECT cron.schedule_in_database('wipe', '1 seconds', "+
			"$$DELETE FROM orders WHERE now() > '%s+00'$$, '%s');", deadline, dbName)); err != nil {
		return fmt.Errorf("schedule job: %w", err)
	}

	log.Println("Step 3: A physical snapshot, taken before the deadline")
	if output, err = h.runCLI("snapshot", "make"); err != nil {
		return fmt.Errorf("snapshot make: %w (output: %s)", err, output)
	}
	records, err := listSnapshotRecords(h)
	if err != nil {
		return err
	}
	snapshotID := strconv.Itoa(records[0].ID)
	if n, err := countRows(port, dbName, password, "orders"); err != nil || n != rows {
		return fmt.Errorf("the job fired before the snapshot (%d rows, %v); the deadline is too close", n, err)
	}

	log.Println("Step 4: Control — the job really runs, and empties the LIVE table")
	var live int
	for range 60 {
		if live, err = countRows(port, dbName, password, "orders"); err == nil && live == 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if live != 0 {
		return fmt.Errorf("control failed: the pg_cron job never emptied the live table (%d rows left); the test proves nothing", live)
	}

	log.Println("Step 5: Restoring from the snapshot keeps every row: the scratch copy ran no job")
	output, err = h.runCLI("snapshot", "restore-database", "--instance", instanceName,
		"--database", dbName, "--restore-as", "cronsales_restored", "--id", snapshotID, "--yes")
	if err != nil {
		return fmt.Errorf("restore-database: %w (output: %s)", err, output)
	}
	if n, err := countRows(port, "cronsales_restored", password, "orders"); err != nil || n != rows {
		return fmt.Errorf("restored copy has %d rows (%v), want %d: the snapshot's pg_cron job ran inside the scratch cluster before the dump", n, err, rows)
	}
	if err := expectNoScratchLeftovers(); err != nil {
		return err
	}

	log.Println("=== Snapshot Restore Database scratch-jobs Test PASSED ===")
	return nil
}

// ensurePgCronImage builds pgCronImage if it is not present. It is cached by
// Docker afterwards, so only the first run pays for the apt install.
func ensurePgCronImage() error {
	if err := exec.Command("docker", "image", "inspect", pgCronImage).Run(); err == nil {
		return nil
	}
	log.Printf("Building %s (first run only)...", pgCronImage)
	cmd := exec.Command("docker", "build", "-t", pgCronImage, "-")
	cmd.Stdin = strings.NewReader("FROM postgres:17\n" +
		"RUN apt-get update && apt-get install -y --no-install-recommends postgresql-17-cron && rm -rf /var/lib/apt/lists/*\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build %s: %w\n%s", pgCronImage, err, out)
	}
	return nil
}

// testSnapshotRestoreDatabaseLoginTrigger proves the clients that talk to the
// scratch copy never fire a PostgreSQL 17+ LOGIN event trigger.
//
// A login trigger runs when a client connects — the readiness probe, the
// metadata query and pg_dump all do — so it needs no background worker and is
// untouched by max_worker_processes=0. The server-level read-only default does
// not stop it either: a role-level `default_transaction_read_only = off`
// outranks it. Client connection options outrank both, which is what the
// scratch clients use.
func testSnapshotRestoreDatabaseLoginTrigger(h *TestHarness) error {
	log.Println("=== Testing Snapshot Restore Database: scratch clients fire no login trigger ===")

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}
	const (
		port   = 15514
		dbName = "loginsales"
		rows   = 100
	)
	instanceName := fmt.Sprintf("oddk-danger-funct-login-%d", time.Now().Unix())

	log.Println("Step 1: An instance whose database wipes a table on login, after a deadline")
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
	if output, err = h.createDatabaseCLI(instanceName, dbName); err != nil {
		return fmt.Errorf("create database: %w (output: %s)", err, output)
	}
	password, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return err
	}
	password = strings.TrimSpace(password)
	deadline := time.Now().UTC().Add(20 * time.Second).Format("2006-01-02 15:04:05")
	if err := h.execSQLAsUser(port, dbName, "postgres", password, fmt.Sprintf(`
		CREATE TABLE orders (id int primary key, note text);
		INSERT INTO orders SELECT g, 'snapshot-time' FROM generate_series(1, %d) g;
		CREATE FUNCTION wipe_on_login() RETURNS event_trigger LANGUAGE plpgsql AS $f$
		BEGIN
			DELETE FROM public.orders WHERE now() > '%s+00';
		END $f$;
		CREATE EVENT TRIGGER wipe_on_login ON login EXECUTE FUNCTION wipe_on_login();
		ALTER EVENT TRIGGER wipe_on_login ENABLE ALWAYS;
		ALTER ROLE postgres SET default_transaction_read_only = off;`, rows, deadline)); err != nil {
		return fmt.Errorf("seed: %w", err)
	}

	log.Println("Step 2: A physical snapshot, taken before the deadline")
	if output, err = h.runCLI("snapshot", "make"); err != nil {
		return fmt.Errorf("snapshot make: %w (output: %s)", err, output)
	}
	records, err := listSnapshotRecords(h)
	if err != nil {
		return err
	}
	snapshotID := strconv.Itoa(records[0].ID)

	log.Println("Step 3: Control — after the deadline, a login empties the LIVE table")
	var live int
	for range 60 {
		// Each countRows call is itself a login to the database.
		if live, err = countRows(port, dbName, password, "orders"); err == nil && live == 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if live != 0 {
		return fmt.Errorf("control failed: the login trigger never emptied the live table (%d rows left); the test proves nothing", live)
	}

	log.Println("Step 4: Restoring from the snapshot keeps every row: no scratch client fired the trigger")
	output, err = h.runCLI("snapshot", "restore-database", "--instance", instanceName,
		"--database", dbName, "--restore-as", "loginsales_restored", "--id", snapshotID, "--yes")
	if err != nil {
		return fmt.Errorf("restore-database: %w (output: %s)", err, output)
	}
	// The restored database carries the event trigger too (pg_dump dumps it),
	// so it is counted through a connection with event triggers off.
	n, err := countRowsNoEventTriggers(port, "loginsales_restored", password, "orders")
	if err != nil || n != rows {
		return fmt.Errorf("restored copy has %d rows (%v), want %d: a login trigger fired inside the scratch cluster before the dump", n, err, rows)
	}
	if err := expectNoScratchLeftovers(); err != nil {
		return err
	}

	log.Println("=== Snapshot Restore Database login-trigger Test PASSED ===")
	return nil
}

// countRowsNoEventTriggers counts rows over a connection that cannot fire a
// login trigger — the restored database carries the trigger too, and counting
// through it would delete the very rows being counted.
func countRowsNoEventTriggers(port int, dbName, password, table string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, fmt.Sprintf(
		"postgres://postgres:%s@10.88.0.1:%d/%s?sslmode=disable&options=-c%%20event_triggers%%3Doff", password, port, dbName))
	if err != nil {
		return 0, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// testRestoreInstanceLoginTrigger proves a whole-instance restore makes no
// connection that fires the restored cluster's own LOGIN trigger
// (PostgreSQL 17+), for both archive formats.
//
// The trigger AUDITS logins into a table rather than deleting anything, so the
// assertion is exact: health checks for the instance are paused while
// restore-instance runs, so any audit row stamped inside the restore's window
// was written by one of ODDK's own restore-time connections — the readiness
// probe and database count after a physical restore, the final verification
// after a logical one. (After the restore, the cluster's triggers fire on
// logins exactly as they did on the source — that is the cluster's behaviour,
// not the restore's.)
func testRestoreInstanceLoginTrigger(h *TestHarness) error {
	log.Println("=== Testing Snapshot Restore Instance: restore connections fire no login trigger ===")

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}
	const port = 15515
	instanceName := fmt.Sprintf("oddk-danger-funct-rilogin-%d", time.Now().Unix())

	log.Println("Step 1: An instance whose postgres database audits every login")
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
	password, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return err
	}
	password = strings.TrimSpace(password)
	if err := h.execSQLAsUser(port, "postgres", "postgres", password, `
		CREATE TABLE public.login_audit (at timestamptz NOT NULL DEFAULT clock_timestamp());
		CREATE FUNCTION audit_login() RETURNS event_trigger LANGUAGE plpgsql AS $f$
		BEGIN
			INSERT INTO public.login_audit DEFAULT VALUES;
		END $f$;
		CREATE EVENT TRIGGER audit_login ON login EXECUTE FUNCTION audit_login();
		ALTER EVENT TRIGGER audit_login ENABLE ALWAYS;`); err != nil {
		return fmt.Errorf("seed: %w", err)
	}

	for _, format := range []string{"physical", "logical"} {
		log.Printf("Step 2 (%s): snapshot, then restore-instance", format)
		args := []string{"snapshot", "make"}
		if format == "logical" {
			args = append(args, "--logical")
		}
		if output, err = h.runCLI(args...); err != nil {
			return fmt.Errorf("snapshot make (%s): %w (output: %s)", format, err, output)
		}
		records, err := listSnapshotRecords(h)
		if err != nil {
			return err
		}
		start := time.Now()
		output, err = h.runCLI("snapshot", "restore-instance", "--instance", instanceName,
			"--id", strconv.Itoa(records[0].ID), "--yes")
		end := time.Now()
		if err != nil {
			return fmt.Errorf("restore-instance (%s): %w (output: %s)", format, err, output)
		}

		log.Printf("Step 3 (%s): no login was audited during the restore", format)
		fired, err := countAuditedLogins(port, password, start, end)
		if err != nil {
			return err
		}
		if fired != 0 {
			return fmt.Errorf("%s restore-instance fired the restored cluster's login trigger %d time(s) from its own connections", format, fired)
		}
	}

	log.Println("=== Snapshot Restore Instance login-trigger Test PASSED ===")
	return nil
}

// countAuditedLogins counts audit rows stamped inside [from, to], over a
// connection that itself cannot fire the trigger.
func countAuditedLogins(port int, password string, from, to time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, fmt.Sprintf(
		"postgres://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable&options=-c%%20event_triggers%%3Doff", password, port))
	if err != nil {
		return 0, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	err = conn.QueryRow(ctx, "SELECT count(*) FROM public.login_audit WHERE at BETWEEN $1 AND $2", from, to).Scan(&n)
	return n, err
}
