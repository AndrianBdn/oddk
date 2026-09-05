//go:build oddk_debug

package main

import (
	"context"
	"fmt"
	"strings"
)

func testDeleteUserConcurrentDDL(h *TestHarness) error {
	const port = 15477
	name := testPrefix + "-delete-race"
	if out, err := h.createInstanceWithImageCLI(name, port, "postgres:17", "17"); err != nil {
		return fmt.Errorf("create instance: %w (%s)", err, out)
	}
	pw, err := h.getPasswordCLI(name, "--plain")
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := pgConnect(port, "postgres", strings.TrimSpace(pw), "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	// dblink commits on a second connection exactly when DROP OWNED starts,
	// after the production operation has already reassigned existing objects.
	// Unlike a sleep-based race, this reproduces the lost-table window every run.
	_, err = conn.Exec(ctx, `
		CREATE EXTENSION dblink;
		CREATE ROLE departing LOGIN;
		ALTER DATABASE postgres OWNER TO departing;
		CREATE TABLE preserved_before (id int);
		INSERT INTO preserved_before VALUES (7);
		ALTER TABLE preserved_before OWNER TO departing;
		ALTER DEFAULT PRIVILEGES FOR ROLE departing GRANT SELECT ON TABLES TO PUBLIC;
		CREATE FOREIGN DATA WRAPPER deletion_test_fdw;
		CREATE SERVER deletion_test_server FOREIGN DATA WRAPPER deletion_test_fdw;
		CREATE USER MAPPING FOR departing SERVER deletion_test_server;
		CREATE FUNCTION inject_concurrent_ddl() RETURNS event_trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM dblink_exec('dbname=postgres user=postgres',
				'CREATE TABLE committed_during_delete (id int); INSERT INTO committed_during_delete VALUES (42); ALTER TABLE committed_during_delete OWNER TO departing');
		END $$;
		CREATE EVENT TRIGGER inject_concurrent_ddl ON ddl_command_start
			WHEN TAG IN ('DROP OWNED') EXECUTE FUNCTION inject_concurrent_ddl();
	`)
	if err != nil {
		return fmt.Errorf("seed deletion race: %w", err)
	}

	out, err := h.deleteDatabaseUserCLI(name, "departing")
	if err == nil || !strings.Contains(err.Error()+out, "concurrent object creation") {
		return fmt.Errorf("deletion must refuse concurrent object loss, got %v (%s)", err, out)
	}
	var before, during int
	if err := conn.QueryRow(ctx, "SELECT id FROM preserved_before").Scan(&before); err != nil {
		return fmt.Errorf("original data lost: %w", err)
	}
	if err := conn.QueryRow(ctx, "SELECT id FROM committed_during_delete").Scan(&during); err != nil {
		return fmt.Errorf("concurrently committed data lost: %w", err)
	}
	if before != 7 || during != 42 {
		return fmt.Errorf("unexpected preserved data: %d, %d", before, during)
	}
	var owner string
	if err := conn.QueryRow(ctx, "SELECT tableowner FROM pg_tables WHERE tablename = 'preserved_before'").Scan(&owner); err != nil {
		return err
	}
	if owner != "departing" {
		return fmt.Errorf("failed cleanup must roll ownership back, got %q", owner)
	}
	if err := conn.QueryRow(ctx, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'postgres'").Scan(&owner); err != nil {
		return err
	}
	if owner != "departing" {
		return fmt.Errorf("failed cleanup must roll database ownership back, got %q", owner)
	}
	var guards int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_event_trigger WHERE evtname = 'oddk_preserve_owned_objects'").Scan(&guards); err != nil {
		return err
	}
	if guards != 0 {
		return fmt.Errorf("failed deletion leaked its event trigger")
	}

	if _, err := conn.Exec(ctx, "DROP EVENT TRIGGER inject_concurrent_ddl; DROP FUNCTION inject_concurrent_ddl()"); err != nil {
		return err
	}
	if out, err := h.deleteDatabaseUserCLI(name, "departing"); err != nil {
		return fmt.Errorf("retry without concurrent DDL: %w (%s)", err, out)
	}
	var remaining int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname = 'departing'`).Scan(&remaining); err != nil {
		return err
	}
	if remaining != 0 {
		return fmt.Errorf("successful deletion left the role behind")
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_tables
		WHERE tablename IN ('preserved_before', 'committed_during_delete') AND tableowner = 'postgres'`).Scan(&remaining); err != nil {
		return err
	}
	if remaining != 2 {
		return fmt.Errorf("successful retry must preserve and reassign both tables, got %d", remaining)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_event_trigger WHERE evtname = 'oddk_preserve_owned_objects'").Scan(&guards); err != nil {
		return err
	}
	if guards != 0 {
		return fmt.Errorf("successful deletion leaked its event trigger")
	}
	return nil
}
