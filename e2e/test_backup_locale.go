package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// testBackupRestoreLocale verifies that a non-default (C) locale database keeps
// its encoding/collation when restored — i.e. the backup records per-database
// metadata (databases.json) and restore recreates the database from it instead
// of with the cluster defaults.
func testBackupRestoreLocale(h *TestHarness) error {
	log.Println("=== Testing Backup/Restore Locale Preservation ===")

	instanceName := testPrefix + "-backup-locale"
	port := 15447
	ctx := context.Background()

	log.Println("Step 1: Pulling PostgreSQL 17 image")
	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	log.Println("Step 2: Creating PG17 instance")
	output, err := h.runCLI("create",
		"--name", instanceName,
		"--version", "17",
		"--port", strconv.Itoa(port),
		"--cpu", "1",
		"--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance failed: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	pwOut, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("get password failed: %w", err)
	}
	password := strings.TrimSpace(pwOut)

	log.Println("Step 3: Seeding a C-locale database")
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "postgres")
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "CREATE DATABASE clocaledb TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
			return fmt.Errorf("create clocaledb: %w", err)
		}
		return nil
	}(); err != nil {
		return err
	}
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "clocaledb")
		if err != nil {
			return fmt.Errorf("connect clocaledb: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "CREATE TABLE t (id int PRIMARY KEY)"); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
		if _, err := conn.Exec(ctx, "INSERT INTO t VALUES (1),(2),(3)"); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		return nil
	}(); err != nil {
		return err
	}

	log.Println("Step 4: Creating backup")
	if _, err := h.runCLI("backup", "make", instanceName); err != nil {
		return fmt.Errorf("backup make failed: %w", err)
	}
	backupID, err := h.firstBackupID(instanceName)
	if err != nil {
		return err
	}

	log.Println("Step 5: Restoring clocaledb as clocaledb_r")
	output, err = h.runCLI("backup", "restore",
		"--instance", instanceName,
		"--id", strconv.Itoa(backupID),
		"--database", "clocaledb",
		"--restore-as", "clocaledb_r")
	if err != nil {
		return fmt.Errorf("restore failed: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Successfully restored") {
		return fmt.Errorf("expected success message, got: %s", output)
	}

	// The restored database must keep C/C collation (not the cluster default), with data intact.
	log.Println("Step 6: Verifying restored database kept C locale and data")
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "postgres")
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		var collate, ctype string
		if err := conn.QueryRow(ctx, "SELECT datcollate, datctype FROM pg_database WHERE datname='clocaledb_r'").Scan(&collate, &ctype); err != nil {
			return fmt.Errorf("query restored locale: %w", err)
		}
		if collate != "C" || ctype != "C" {
			return fmt.Errorf("expected restored DB to keep C/C collation, got collate=%q ctype=%q", collate, ctype)
		}
		return nil
	}(); err != nil {
		return err
	}
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "clocaledb_r")
		if err != nil {
			return fmt.Errorf("connect restored db: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		var n int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
			return fmt.Errorf("count rows: %w", err)
		}
		if n != 3 {
			return fmt.Errorf("expected 3 rows in restored t, got %d", n)
		}
		return nil
	}(); err != nil {
		return err
	}

	log.Println("Step 7: Cleaning up")
	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	log.Println("=== Backup/Restore Locale Preservation Test PASSED ===")
	return nil
}

// firstBackupID returns the lowest backup id listed for an instance by parsing
// the `backup list` CLI output (first field of the first data row).
func (h *TestHarness) firstBackupID(instanceName string) (int, error) {
	output, err := h.runCLI("backup", "list", "--instance", instanceName)
	if err != nil {
		return 0, fmt.Errorf("list backups failed: %w (output: %s)", err, output)
	}
	for i, line := range strings.Split(output, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue // header / blank
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if id, err := strconv.Atoi(fields[0]); err == nil {
			return id, nil
		}
	}
	return 0, fmt.Errorf("could not find a backup id in output: %s", output)
}
