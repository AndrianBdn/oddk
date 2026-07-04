package operations

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hypersequent/oddk/internal/operr"
	"github.com/hypersequent/oddk/internal/util"
)

type CreateDatabaseParams struct {
	InstanceName string
	DatabaseName string
	// Username, when set, also creates the database's owner user in the
	// same operation — the right setup for a service that runs its own
	// migrations. If database creation fails, the just-created user is
	// rolled back (best-effort; a failed rollback is logged and reported).
	// Other access levels are deliberately not offered here — a read-only
	// user on a brand-new empty database has nothing to read; add those
	// with add-db-user once the database has content.
	Username string
}

type CreateDatabaseResult struct {
	DatabaseName string `json:"databaseName"`
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
	Message      string `json:"message"`
}

func CreateDatabase(ctx context.Context, deps *Dependencies, params CreateDatabaseParams) (*CreateDatabaseResult, error) {
	if params.InstanceName == "" {
		return nil, fmt.Errorf("instance name is required")
	}
	if params.DatabaseName == "" {
		return nil, fmt.Errorf("database name is required")
	}

	conn, err := ConnectToRunningInstance(ctx, deps, params.InstanceName)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = conn.Close(ctx)
	}()

	// Check if database already exists
	var exists bool
	checkQuery := "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)"
	if err := conn.QueryRow(ctx, checkQuery, params.DatabaseName).Scan(&exists); err != nil {
		return nil, fmt.Errorf("failed to check if database exists: %w", err)
	}

	if exists {
		return nil, operr.Conflictf("database %s already exists", params.DatabaseName)
	}

	// Fail on a taken username before creating anything
	if params.Username != "" {
		var userExists bool
		checkUserQuery := "SELECT EXISTS(SELECT 1 FROM pg_user WHERE usename = $1)"
		if err := conn.QueryRow(ctx, checkUserQuery, params.Username).Scan(&userExists); err != nil {
			return nil, fmt.Errorf("failed to check if user exists: %w", err)
		}
		if userExists {
			return nil, operr.Conflictf("user %s already exists", params.Username)
		}
	}

	var password string
	if params.Username != "" {
		password = util.GenerateSecurePassword(24)
		// PostgreSQL doesn't accept parameter binding for the PASSWORD clause,
		// so the literal is escaped via quotePostgresLiteral instead.
		createUserQuery := fmt.Sprintf("CREATE USER %s WITH PASSWORD %s",
			pgx.Identifier{params.Username}.Sanitize(), quotePostgresLiteral(password))
		if _, err := conn.Exec(ctx, createUserQuery); err != nil {
			return nil, fmt.Errorf("failed to create user: %w", err)
		}
	}

	// The OWNER clause makes the fresh database fully owned by the new user,
	// including the public schema (owned by pg_database_owner since PG 15) —
	// no grants or ownership transfers needed, unlike add-db-user --owner on
	// an existing database.
	createQuery := fmt.Sprintf("CREATE DATABASE %s", pgx.Identifier{params.DatabaseName}.Sanitize())
	if params.Username != "" {
		createQuery += fmt.Sprintf(" OWNER %s", pgx.Identifier{params.Username}.Sanitize())
	}
	if _, err := conn.Exec(ctx, createQuery); err != nil {
		if params.Username != "" {
			if dropErr := dropUserBestEffort(ctx, conn, params.Username); dropErr != nil {
				return nil, fmt.Errorf("failed to create database: %w (user %s was created but could not be rolled back — remove it with delete-db-user)", err, params.Username)
			}
		}
		return nil, fmt.Errorf("failed to create database: %w", err)
	}

	message := fmt.Sprintf("Database %s created successfully", params.DatabaseName)
	if params.Username != "" {
		message = fmt.Sprintf("Database %s and owner user %s created successfully",
			params.DatabaseName, params.Username)
	}

	return &CreateDatabaseResult{
		DatabaseName: params.DatabaseName,
		Username:     params.Username,
		Password:     password,
		Message:      message,
	}, nil
}
