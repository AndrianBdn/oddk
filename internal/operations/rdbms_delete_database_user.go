package operations

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"

	"github.com/andrianbdn/oddk/internal/operr"
)

type DeleteDatabaseUserParams struct {
	InstanceName string
	Username     string
}

type DeleteDatabaseUserResult struct {
	Username string `json:"username"`
	Message  string `json:"message"`
}

func DeleteDatabaseUser(ctx context.Context, deps *Dependencies, params DeleteDatabaseUserParams) (*DeleteDatabaseUserResult, error) {
	if params.InstanceName == "" {
		return nil, fmt.Errorf("instance name is required")
	}
	if params.Username == "" {
		return nil, fmt.Errorf("username is required")
	}

	// Prevent deletion of postgres superuser
	if params.Username == "postgres" {
		return nil, operr.Forbiddenf("cannot delete postgres superuser")
	}

	conn, err := ConnectToRunningInstance(ctx, deps, params.InstanceName)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = conn.Close(ctx)
	}()

	// Check if user exists
	var userExists bool
	checkUserQuery := "SELECT EXISTS(SELECT 1 FROM pg_user WHERE usename = $1)"
	if err := conn.QueryRow(ctx, checkUserQuery, params.Username).Scan(&userExists); err != nil {
		return nil, fmt.Errorf("failed to check if user exists: %w", err)
	}

	if !userExists {
		return nil, operr.NotFoundf("user %s does not exist", params.Username)
	}

	dbQuery := `
		SELECT datname 
		FROM pg_database 
		WHERE datistemplate = false
	`
	rows, err := conn.Query(ctx, dbQuery)
	if err != nil {
		return nil, fmt.Errorf("failed to list databases: %w", err)
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			return nil, fmt.Errorf("failed to scan database name: %w", err)
		}
		databases = append(databases, dbName)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating database rows: %w", err)
	}

	// Process each database to reassign ownership and revoke privileges
	for _, dbName := range databases {
		// Connect to the database to handle object ownership
		dbConn, err := ConnectToRunningInstance(ctx, deps, params.InstanceName, ConnectOptions{Database: dbName})
		if err != nil {
			log.Printf("Warning: cannot connect to database %s: %v", dbName, err)
			continue
		}

		if err := reassignAndRevokeDatabaseUser(ctx, dbConn, params.Username); err != nil {
			_ = dbConn.Close(ctx)
			return nil, fmt.Errorf("preserve objects and revoke privileges for %s in database %s (user was NOT deleted; "+
				"on an out-of-shared-memory error raise max_locks_per_transaction; on concurrent object creation stop the application's DDL and retry): %w",
				params.Username, dbName, err)
		}

		_ = dbConn.Close(ctx)
	}

	// Finally, drop the user
	dropUserQuery := fmt.Sprintf("DROP USER %s",
		pgx.Identifier{params.Username}.Sanitize())
	if _, err := conn.Exec(ctx, dropUserQuery); err != nil {
		return nil, fmt.Errorf("failed to drop user: %w", err)
	}

	return &DeleteDatabaseUserResult{
		Username: params.Username,
		Message:  fmt.Sprintf("User %s deleted successfully (owned objects reassigned to postgres)", params.Username),
	}, nil
}

// reassignAndRevokeDatabaseUser preserves objects even if another session
// creates one after REASSIGN OWNED's scan. A transaction alone cannot prevent
// that race: DROP OWNED would see the new object and delete it. The sql_drop
// guard aborts that transaction instead. It exists only inside this transaction
// and is removed before commit, so other sessions never acquire the guard.
func reassignAndRevokeDatabaseUser(ctx context.Context, conn *pgx.Conn, username string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Default ACLs and user mappings belong to the departing role and are
	// deliberately not reassigned by PostgreSQL. They are privilege metadata;
	// every other object deletion must roll back, including dependent objects.
	_, err = tx.Exec(ctx, `
		DO $$ BEGIN
			IF current_setting('event_triggers', true) IS NOT NULL THEN
				PERFORM set_config('event_triggers', 'on', true);
			END IF;
		END $$;
		CREATE FUNCTION pg_temp.oddk_preserve_owned_objects() RETURNS event_trigger
		LANGUAGE plpgsql AS $$
		DECLARE dropped record;
		BEGIN
			FOR dropped IN SELECT * FROM pg_catalog.pg_event_trigger_dropped_objects()
			LOOP
				IF dropped.classid NOT IN ('pg_catalog.pg_default_acl'::regclass, 'pg_catalog.pg_user_mapping'::regclass) THEN
					RAISE EXCEPTION 'concurrent object creation: refusing to delete % %',
						dropped.object_type, dropped.object_identity;
				END IF;
			END LOOP;
		END $$;
		CREATE EVENT TRIGGER oddk_preserve_owned_objects ON sql_drop
			WHEN TAG IN ('DROP OWNED') EXECUTE FUNCTION pg_temp.oddk_preserve_owned_objects();
		ALTER EVENT TRIGGER oddk_preserve_owned_objects ENABLE ALWAYS;
	`)
	if err != nil {
		return fmt.Errorf("install object preservation guard: %w", err)
	}
	user := pgx.Identifier{username}.Sanitize()
	// This also reassigns shared objects (databases and tablespaces), keeping
	// their ownership changes inside the same rollback as the local objects.
	if _, err := tx.Exec(ctx, "REASSIGN OWNED BY "+user+" TO postgres"); err != nil {
		return fmt.Errorf("reassign ownership: %w", err)
	}
	if _, err := tx.Exec(ctx, "DROP OWNED BY "+user); err != nil {
		return fmt.Errorf("revoke privileges: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DROP EVENT TRIGGER oddk_preserve_owned_objects;
		DROP FUNCTION pg_temp.oddk_preserve_owned_objects();
	`); err != nil {
		return fmt.Errorf("remove object preservation guard: %w", err)
	}
	return tx.Commit(ctx)
}
