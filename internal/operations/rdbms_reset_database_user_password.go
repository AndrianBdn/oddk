package operations

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hypersequent/oddk/internal/operr"
	"github.com/hypersequent/oddk/internal/util"
)

type ResetDatabaseUserPasswordParams struct {
	InstanceName string
	Username     string
}

type ResetDatabaseUserPasswordResult struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Message  string `json:"message"`
}

func ResetDatabaseUserPassword(ctx context.Context, deps *Dependencies, params ResetDatabaseUserPasswordParams) (*ResetDatabaseUserPasswordResult, error) {
	if params.InstanceName == "" {
		return nil, fmt.Errorf("instance name is required")
	}
	if params.Username == "" {
		return nil, fmt.Errorf("username is required")
	}

	// Prevent resetting postgres superuser password
	if params.Username == "postgres" {
		return nil, operr.Forbiddenf("cannot reset postgres superuser password")
	}

	// Connect to any database (we'll use postgres)
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

	// Generate a new secure password
	newPassword := util.GenerateSecurePassword(24)

	// Reset the user's password. PostgreSQL doesn't accept parameter binding
	// for the PASSWORD clause, so the literal is escaped via quotePostgresLiteral.
	alterUserQuery := fmt.Sprintf("ALTER USER %s WITH PASSWORD %s",
		pgx.Identifier{params.Username}.Sanitize(), quotePostgresLiteral(newPassword))
	if _, err := conn.Exec(ctx, alterUserQuery); err != nil {
		return nil, fmt.Errorf("failed to reset password: %w", err)
	}

	return &ResetDatabaseUserPasswordResult{
		Username: params.Username,
		Password: newPassword,
		Message:  fmt.Sprintf("Password reset successfully for user %s", params.Username),
	}, nil
}
