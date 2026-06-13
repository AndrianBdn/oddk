package operations

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hypersequent/oddk/internal/operr"
)

// quotePostgresLiteral wraps s in single quotes and doubles any embedded
// single quotes, producing a safe PostgreSQL string literal. Used for
// statements that don't accept parameter binding (CREATE USER, ALTER USER
// WITH PASSWORD '...'). Assumes standard_conforming_strings=on, the default
// since PostgreSQL 9.1, so backslashes need no special handling.
func quotePostgresLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// PostgreSQL connectivity status constants
type PostgreSQLStatus int

const (
	PostgreSQLStatusOK PostgreSQLStatus = iota
	PostgreSQLStatusBrokenPort
	PostgreSQLStatusBrokenAuth
	PostgreSQLStatusOther
)

// ConnectOptions allows specifying optional parameters for database connections
type ConnectOptions struct {
	Database string
}

// ConnectToRunningInstance connects to a PostgreSQL instance, ensuring it's running first
func ConnectToRunningInstance(ctx context.Context, deps *Dependencies, instanceName string, opts ...ConnectOptions) (*pgx.Conn, error) {
	instance, err := deps.Store.Instances.Get(instanceName)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}

	if instance.Status != "running" {
		return nil, operr.Invalidf("instance %s is not running (status: %s)", instanceName, instance.Status)
	}

	password, err := deps.Store.Instances.GetDecryptedPassword(instanceName, deps.MasterKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt password: %w", err)
	}

	// Default to postgres database
	database := "postgres"
	if len(opts) > 0 && opts[0].Database != "" {
		database = opts[0].Database
	}

	connStr := fmt.Sprintf("postgres://postgres:%s@10.88.0.1:%d/%s?sslmode=disable",
		password, instance.Port, database)

	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}

	return conn, nil
}

// TestPostgreSQLConnectivity tests PostgreSQL connectivity and returns detailed status
func TestPostgreSQLConnectivity(ctx context.Context, deps *Dependencies, instanceName string) PostgreSQLStatus {
	// Set a reasonable timeout for health check
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	instance, err := deps.Store.Instances.Get(instanceName)
	if err != nil {
		return PostgreSQLStatusOther
	}

	password, err := deps.Store.Instances.GetDecryptedPassword(instanceName, deps.MasterKey)
	if err != nil {
		return PostgreSQLStatusOther
	}

	// Optimistically try PostgreSQL connection first
	connStr := fmt.Sprintf("postgres://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable",
		password, instance.Port)

	pgConn, err := pgx.Connect(checkCtx, connStr)
	if err != nil {
		// Connection failed, determine the reason

		// Check if this is an authentication error
		if strings.Contains(err.Error(), "password authentication failed") ||
			strings.Contains(err.Error(), "authentication failed") ||
			strings.Contains(err.Error(), "role") && strings.Contains(err.Error(), "does not exist") {
			return PostgreSQLStatusBrokenAuth
		}

		// Check if we can connect to the port (network layer)
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("10.88.0.1:%d", instance.Port), 3*time.Second)
		if err != nil {
			return PostgreSQLStatusBrokenPort
		}
		_ = conn.Close()

		// Port is accessible but PostgreSQL connection failed for other reasons
		return PostgreSQLStatusOther
	}
	defer func() {
		_ = pgConn.Close(checkCtx)
	}()

	// Perform actual PostgreSQL ping
	if err := pgConn.Ping(checkCtx); err != nil {
		return PostgreSQLStatusOther
	}

	return PostgreSQLStatusOK
}

// TestPostgreSQLConnectivityWithPassword tests PostgreSQL connectivity with a custom password
func TestPostgreSQLConnectivityWithPassword(ctx context.Context, port int, password string) error {
	// Set a reasonable timeout for connection test
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	connStr := fmt.Sprintf("postgres://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable",
		password, port)

	// Try to connect with the provided password
	pgConn, err := pgx.Connect(checkCtx, connStr)
	if err != nil {
		if strings.Contains(err.Error(), "password authentication failed") ||
			strings.Contains(err.Error(), "authentication failed") {
			return fmt.Errorf("authentication failed with provided password")
		}

		// Check if we can connect to the port (network layer)
		conn, netErr := net.DialTimeout("tcp", fmt.Sprintf("10.88.0.1:%d", port), 3*time.Second)
		if netErr != nil {
			return fmt.Errorf("PostgreSQL port %d is not accessible", port)
		}
		_ = conn.Close()

		// Port is accessible but PostgreSQL connection failed for other reasons
		return fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}
	defer func() {
		_ = pgConn.Close(checkCtx)
	}()

	// Perform actual PostgreSQL ping
	if err := pgConn.Ping(checkCtx); err != nil {
		return fmt.Errorf("failed to ping PostgreSQL: %w", err)
	}

	return nil
}
