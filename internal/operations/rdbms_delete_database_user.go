package operations

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"

	"github.com/hypersequent/oddk/internal/operr"
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
		// Check if user owns this database
		var dbOwner string
		ownerQuery := "SELECT pg_catalog.pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1"
		if err := conn.QueryRow(ctx, ownerQuery, dbName).Scan(&dbOwner); err != nil {
			log.Printf("Warning: failed to check database owner for %s: %v", dbName, err)
			continue
		}

		// If user owns the database, reassign to postgres
		if dbOwner == params.Username {
			alterDBQuery := fmt.Sprintf("ALTER DATABASE %s OWNER TO postgres",
				pgx.Identifier{dbName}.Sanitize())
			if _, err := conn.Exec(ctx, alterDBQuery); err != nil {
				return nil, fmt.Errorf("failed to reassign database %s ownership: %w", dbName, err)
			}
			log.Printf("Reassigned database %s ownership from %s to postgres", dbName, params.Username)
		}

		// Connect to the database to handle object ownership
		dbConn, err := ConnectToRunningInstance(ctx, deps, params.InstanceName, ConnectOptions{Database: dbName})
		if err != nil {
			log.Printf("Warning: cannot connect to database %s: %v", dbName, err)
			continue
		}

		// Reassign all owned objects in this database to postgres
		reassignQuery := fmt.Sprintf("REASSIGN OWNED BY %s TO postgres",
			pgx.Identifier{params.Username}.Sanitize())
		if _, err := dbConn.Exec(ctx, reassignQuery); err != nil {
			log.Printf("Warning: failed to reassign owned objects in database %s: %v", dbName, err)
		}

		// Drop any privileges granted to the user (but not the objects themselves)
		dropOwnedQuery := fmt.Sprintf("DROP OWNED BY %s",
			pgx.Identifier{params.Username}.Sanitize())
		if _, err := dbConn.Exec(ctx, dropOwnedQuery); err != nil {
			log.Printf("Warning: failed to drop privileges in database %s: %v", dbName, err)
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
