package operations

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/util"
)

// quotePostgresLiteral wraps s in single quotes and doubles any embedded
// single quotes, producing a safe PostgreSQL string literal. Used for
// statements that don't accept parameter binding (CREATE USER, ALTER USER
// WITH PASSWORD '...'). Assumes standard_conforming_strings=on, the default
// since PostgreSQL 9.1, so backslashes need no special handling.
func quotePostgresLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// newInitdbCredential returns the throwaway superuser password a fresh cluster
// is initialised with.
//
// The instance's real password must never reach POSTGRES_PASSWORD, because
// Docker keeps Config.Env for the container's whole lifetime (see
// docker.containerEnv). But the entrypoint has to be given something to run
// initdb with: its only alternative is POSTGRES_HOST_AUTH_METHOD=trust, which
// would open the cluster to anything that can reach the bridge — a far worse
// trade than a visible credential.
//
// So the cluster is initialised with a random value used exactly twice — the
// readiness probe, and the ALTER that replaces it — and never again. What stays
// visible in container metadata is then a password that authenticated for the
// few seconds between initdb and readiness and authenticates nothing after.
func newInitdbCredential() string {
	return util.GenerateSecurePassword(24)
}

// adoptPostgresPassword replaces the cluster's throwaway superuser password
// with the instance's real one, over a connection authenticated by the
// throwaway.
//
// A failure here is fatal to the calling operation, and must stay that way: the
// cluster is up but its superuser password is one ODDK has not stored, so
// leaving it running would produce an instance the daemon can never connect to
// and a health check that reports it broken forever. Every caller either rolls
// the instance back or marks it "error".
func adoptPostgresPassword(ctx context.Context, port int, initdbPassword, realPassword string) error {
	conn, err := connectDirect(ctx, port, initdbPassword)
	if err != nil {
		return fmt.Errorf("connect to set the instance password: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// PostgreSQL does not accept parameter binding for the PASSWORD clause, so
	// the literal is escaped the same way every other ALTER ... PASSWORD here is.
	stmt := "ALTER ROLE postgres WITH PASSWORD " + quotePostgresLiteral(realPassword)
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("set the instance password: %w", err)
	}
	return nil
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

	conn, err := pgx.Connect(ctx, util.PostgresURI(password, instance.Port, database))
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
	pgConn, err := pgx.Connect(checkCtx, util.PostgresURI(password, instance.Port, "postgres"))
	if err != nil {
		// Connection failed, determine the reason

		// Check if this is an authentication error
		if strings.Contains(err.Error(), "password authentication failed") ||
			strings.Contains(err.Error(), "authentication failed") ||
			strings.Contains(err.Error(), "role") && strings.Contains(err.Error(), "does not exist") {
			return PostgreSQLStatusBrokenAuth
		}

		// Check if we can connect to the port (network layer)
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", util.GatewayIP, instance.Port), 3*time.Second)
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

	pgConn, err := pgx.Connect(checkCtx, util.PostgresURI(password, port, "postgres"))
	if err != nil {
		if strings.Contains(err.Error(), "password authentication failed") ||
			strings.Contains(err.Error(), "authentication failed") {
			return fmt.Errorf("authentication failed with provided password")
		}

		// Check if we can connect to the port (network layer)
		conn, netErr := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", util.GatewayIP, port), 3*time.Second)
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
