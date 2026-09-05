//go:build oddk_debug

package main

import (
	"context"
	"fmt"
	"strings"
)

func testDatabaseUserAtomicity(h *TestHarness) error {
	const port = 15478
	name := testPrefix + "-user-atomicity"
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

	// CONNECT succeeds before the missing schema makes the next grant fail.
	if _, err := conn.Exec(ctx, "DROP SCHEMA public"); err != nil {
		return err
	}
	for _, readOnly := range []bool{false, true} {
		out, err := h.addDatabaseUserCLI(name, "grant_candidate", "postgres", readOnly)
		if err == nil || !strings.Contains(err.Error()+out, `schema "public" does not exist`) {
			return fmt.Errorf("expected missing-schema grant failure, got %v (%s)", err, out)
		}
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = 'grant_candidate')").Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("failed grant left its user behind (readOnly=%t)", readOnly)
		}
	}

	_, err = conn.Exec(ctx, `
		CREATE SCHEMA public AUTHORIZATION pg_database_owner;
		CREATE TABLE public.owned_table (id int);
		INSERT INTO public.owned_table VALUES (7);
		CREATE SEQUENCE public.owned_sequence;
		CREATE VIEW public.owned_view AS SELECT id FROM public.owned_table;
		CREATE TYPE public.owned_enum AS ENUM ('one', 'two');
		CREATE TYPE public.owned_composite AS (id int);
		CREATE FUNCTION public.overloaded(int) RETURNS int LANGUAGE SQL AS 'SELECT $1';
		CREATE FUNCTION public.overloaded(text) RETURNS text LANGUAGE SQL AS 'SELECT $1';
		CREATE PROCEDURE public."Mixed Routine"() LANGUAGE SQL AS 'INSERT INTO public.owned_table VALUES (8)';
		CREATE AGGREGATE public.owned_sum(int) (SFUNC = int4pl, STYPE = int, INITCOND = '0');
		CREATE FUNCTION public.fail_ownership() RETURNS event_trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF (SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'postgres') <> 'new_owner'
				OR (SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = 'owned_table') <> 'new_owner' THEN
				RAISE EXCEPTION 'ownership failure injected too early';
			END IF;
			RAISE EXCEPTION 'injected ownership failure';
		END $$;
		CREATE EVENT TRIGGER fail_ownership ON ddl_command_start
			WHEN TAG IN ('ALTER ROUTINE') EXECUTE FUNCTION public.fail_ownership();
		ALTER ROLE postgres IN DATABASE postgres SET search_path = pg_catalog;
	`)
	if err != nil {
		return fmt.Errorf("seed ownership test: %w", err)
	}

	args := []string{"instance", "add-db-user", name, "--username", "new_owner", "--database", "postgres", "--owner"}
	out, err := h.runCLI(args...)
	if err == nil || !strings.Contains(err.Error()+out, "injected ownership failure") {
		return fmt.Errorf("expected injected ownership failure, got %v (%s)", err, out)
	}
	var rolledBack bool
	if err := conn.QueryRow(ctx, `SELECT
		NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = 'new_owner')
		AND (SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'postgres') = 'postgres'
		AND NOT EXISTS(SELECT 1 FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relowner <> 'postgres'::regrole)
		AND NOT EXISTS(SELECT 1 FROM pg_proc WHERE pronamespace = 'public'::regnamespace AND proowner <> 'postgres'::regrole)
	`).Scan(&rolledBack); err != nil {
		return err
	}
	if !rolledBack {
		return fmt.Errorf("failed ownership transfer did not roll back role and ownership changes")
	}
	if _, err := conn.Exec(ctx, "DROP EVENT TRIGGER fail_ownership; DROP FUNCTION public.fail_ownership()"); err != nil {
		return err
	}

	out, err = h.runCLI(args...)
	if err != nil {
		return fmt.Errorf("retry ownership transfer: %w (%s)", err, out)
	}
	var transferred bool
	if err := conn.QueryRow(ctx, `SELECT
		(SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'postgres') = 'new_owner'
		AND NOT EXISTS(SELECT 1 FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relowner <> 'new_owner'::regrole)
		AND NOT EXISTS(SELECT 1 FROM pg_proc WHERE pronamespace = 'public'::regnamespace AND proowner <> 'new_owner'::regrole)
		AND NOT EXISTS(SELECT 1 FROM pg_type WHERE typnamespace = 'public'::regnamespace AND typowner <> 'new_owner'::regrole)
	`).Scan(&transferred); err != nil {
		return err
	}
	if !transferred {
		return fmt.Errorf("ownership transfer missed a table, sequence, view, type, or routine")
	}
	password, err := extractCredentialPassword(out)
	if err != nil {
		return err
	}
	if err := h.execSQLAsUser(port, "postgres", "new_owner", password,
		`ALTER TABLE public.owned_table ADD COLUMN note text; CALL public."Mixed Routine"()`); err != nil {
		return fmt.Errorf("new owner cannot migrate or call its procedure: %w", err)
	}
	var sum int
	if err := conn.QueryRow(ctx, "SELECT public.owned_sum(id) FROM public.owned_view").Scan(&sum); err != nil {
		return err
	}
	if sum != 15 {
		return fmt.Errorf("unexpected data after ownership transfer: %d", sum)
	}
	return nil
}
