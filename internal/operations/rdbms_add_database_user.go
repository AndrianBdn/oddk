package operations

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hypersequent/oddk/internal/operr"
	"github.com/hypersequent/oddk/internal/util"
)

type AddDatabaseUserParams struct {
	InstanceName string
	DatabaseName string
	Username     string
	ReadOnly     bool
	Owner        bool // Make user the owner of the database
}

type AddDatabaseUserResult struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Database string `json:"database"`
	ReadOnly bool   `json:"readOnly"`
	Message  string `json:"message"`
}

func AddDatabaseUser(ctx context.Context, deps *Dependencies, params AddDatabaseUserParams) (*AddDatabaseUserResult, error) {
	if params.InstanceName == "" {
		return nil, fmt.Errorf("instance name is required")
	}
	if params.DatabaseName == "" {
		return nil, fmt.Errorf("database name is required")
	}
	if params.Username == "" {
		return nil, fmt.Errorf("username is required")
	}
	if params.ReadOnly && params.Owner {
		return nil, operr.Invalidf("readOnly and owner are mutually exclusive")
	}

	// Connect directly to the target database - if it doesn't exist, we'll get an error
	conn, err := ConnectToRunningInstance(ctx, deps, params.InstanceName, ConnectOptions{Database: params.DatabaseName})
	if err != nil {
		// Check if it's a database not found error
		if strings.Contains(err.Error(), "database") && strings.Contains(err.Error(), "does not exist") {
			return nil, operr.NotFoundf("database %s does not exist", params.DatabaseName)
		}
		return nil, err
	}
	defer func() {
		_ = conn.Close(ctx)
	}()

	// Check if user already exists (pg_user is accessible from any database)
	var userExists bool
	checkUserQuery := "SELECT EXISTS(SELECT 1 FROM pg_user WHERE usename = $1)"
	if err := conn.QueryRow(ctx, checkUserQuery, params.Username).Scan(&userExists); err != nil {
		return nil, fmt.Errorf("failed to check if user exists: %w", err)
	}

	if userExists {
		return nil, operr.Conflictf("user %s already exists", params.Username)
	}

	// The database owner matters for default privileges: ALTER DEFAULT
	// PRIVILEGES issued as postgres only covers objects postgres itself
	// creates later, so grantStatements must also issue them FOR ROLE the
	// owner (the role that actually creates tables when the database was
	// provisioned via create-db --username).
	var dbOwner string
	ownerQuery := "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = current_database()"
	if err := conn.QueryRow(ctx, ownerQuery).Scan(&dbOwner); err != nil {
		return nil, fmt.Errorf("failed to look up database owner: %w", err)
	}

	// Generate a secure password
	password := util.GenerateSecurePassword(24)

	// Create the user with the generated password (can be done from any database).
	// PostgreSQL doesn't accept parameter binding for the PASSWORD clause,
	// so the literal is escaped via quotePostgresLiteral instead.
	createUserQuery := fmt.Sprintf("CREATE USER %s WITH PASSWORD %s",
		pgx.Identifier{params.Username}.Sanitize(), quotePostgresLiteral(password))
	if _, err := conn.Exec(ctx, createUserQuery); err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	// Apply database- and schema-level grants; the first failure rolls back
	// the just-created user.
	for _, step := range grantStatements(params.DatabaseName, params.Username, params.ReadOnly, dbOwner) {
		if _, err := conn.Exec(ctx, step.sql); err != nil {
			_ = dropUserBestEffort(ctx, conn, params.Username)
			return nil, fmt.Errorf("failed to %s: %w", step.what, err)
		}
	}

	// Transfer ownership if requested
	if params.Owner {
		// Transfer database ownership (must be done from a different database, we're connected to target)
		// We need a separate connection to postgres database for ALTER DATABASE
		postgresConn, err := ConnectToRunningInstance(ctx, deps, params.InstanceName, ConnectOptions{Database: "postgres"})
		if err != nil {
			_ = dropUserBestEffort(ctx, conn, params.Username)
			return nil, fmt.Errorf("failed to connect to postgres database for ownership transfer: %w", err)
		}
		defer func() { _ = postgresConn.Close(ctx) }()

		alterDBQuery := fmt.Sprintf("ALTER DATABASE %s OWNER TO %s",
			pgx.Identifier{params.DatabaseName}.Sanitize(),
			pgx.Identifier{params.Username}.Sanitize())
		if _, err := postgresConn.Exec(ctx, alterDBQuery); err != nil {
			_ = dropUserBestEffort(ctx, conn, params.Username)
			return nil, fmt.Errorf("failed to transfer database ownership: %w", err)
		}

		// Transfer ownership of all objects in the database from postgres to the new user
		// Note: REASSIGN OWNED BY postgres doesn't work because postgres is a superuser
		// and PostgreSQL won't reassign system-required objects. Instead, we transfer
		// ownership of user objects (tables, sequences, functions, etc.) individually.
		if err := transferSchemaObjectsOwnership(ctx, conn, params.Username); err != nil {
			// Note: database ownership already transferred, but that's acceptable
			// The user can still run migrations on their own objects
			return nil, fmt.Errorf("failed to transfer object ownership: %w", err)
		}
	}

	accessType := map[bool]string{true: "read-only", false: "read-write"}[params.ReadOnly]
	if params.Owner {
		accessType = "owner"
	}
	message := fmt.Sprintf("User %s created successfully with %s access to database %s",
		params.Username, accessType, params.DatabaseName)

	return &AddDatabaseUserResult{
		Username: params.Username,
		Password: password,
		Database: params.DatabaseName,
		ReadOnly: params.ReadOnly,
		Message:  message,
	}, nil
}

// sqlStep pairs a statement with the human-readable action used in its error.
type sqlStep struct {
	what string
	sql  string
}

// grantStatements returns the ordered grant statements for a new database
// user: CONNECT for everyone, then either read-only grants (USAGE + SELECT on
// existing objects + SELECT default privileges for future tables) or
// read-write grants (CREATE on the database, ALL on the public schema and
// existing objects, plus ALL default privileges for future tables, sequences
// and functions).
//
// ALTER DEFAULT PRIVILEGES only applies to objects later created BY the role
// that issued it — plain statements run as postgres cover only postgres's
// future objects. When the database is owned by another role (the usual case
// after create-db --username, where the owner runs the app's migrations),
// each default-privileges statement is issued a second time FOR ROLE <owner>
// so this user also covers tables the owner creates in the future.
func grantStatements(databaseName, username string, readOnly bool, dbOwner string) []sqlStep {
	db := pgx.Identifier{databaseName}.Sanitize()
	user := pgx.Identifier{username}.Sanitize()

	forRoles := []string{""} // "" = as the connecting role (postgres)
	if dbOwner != "" && dbOwner != "postgres" && dbOwner != username {
		forRoles = append(forRoles, " FOR ROLE "+pgx.Identifier{dbOwner}.Sanitize())
	}

	steps := []sqlStep{
		{"grant connect permission", fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, user)},
	}

	if readOnly {
		steps = append(steps,
			sqlStep{"grant usage on schema", fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", user)},
			sqlStep{"grant select on tables", fmt.Sprintf("GRANT SELECT ON ALL TABLES IN SCHEMA public TO %s", user)},
			sqlStep{"grant select on sequences", fmt.Sprintf("GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO %s", user)},
		)
		for _, forRole := range forRoles {
			steps = append(steps,
				sqlStep{"set default privileges", fmt.Sprintf("ALTER DEFAULT PRIVILEGES%s IN SCHEMA public GRANT SELECT ON TABLES TO %s", forRole, user)},
			)
		}
		return steps
	}

	steps = append(steps,
		sqlStep{"grant create permission", fmt.Sprintf("GRANT CREATE ON DATABASE %s TO %s", db, user)},
		sqlStep{"grant all on schema", fmt.Sprintf("GRANT ALL ON SCHEMA public TO %s", user)},
		sqlStep{"grant all on tables", fmt.Sprintf("GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO %s", user)},
		sqlStep{"grant all on sequences", fmt.Sprintf("GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO %s", user)},
	)
	for _, forRole := range forRoles {
		steps = append(steps,
			sqlStep{"set default privileges on tables", fmt.Sprintf("ALTER DEFAULT PRIVILEGES%s IN SCHEMA public GRANT ALL ON TABLES TO %s", forRole, user)},
			sqlStep{"set default privileges on sequences", fmt.Sprintf("ALTER DEFAULT PRIVILEGES%s IN SCHEMA public GRANT ALL ON SEQUENCES TO %s", forRole, user)},
			sqlStep{"set default privileges on functions", fmt.Sprintf("ALTER DEFAULT PRIVILEGES%s IN SCHEMA public GRANT ALL ON FUNCTIONS TO %s", forRole, user)},
		)
	}
	return steps
}

// dropUserBestEffort rolls back a partially-configured user. The failure (if
// any) is logged and returned so callers can surface that the user survived,
// but the original error remains what the caller reports.
func dropUserBestEffort(ctx context.Context, conn *pgx.Conn, username string) error {
	dropUserQuery := fmt.Sprintf("DROP USER %s", pgx.Identifier{username}.Sanitize())
	if _, err := conn.Exec(ctx, dropUserQuery); err != nil {
		log.Printf("WARNING: failed to roll back user %s: %v", username, err)
		return err
	}
	return nil
}

// transferSchemaObjectsOwnership transfers ownership of all user objects in public schema
// to the specified user. This is used instead of REASSIGN OWNED BY postgres which fails
// because postgres is a superuser and PostgreSQL protects system objects.
func transferSchemaObjectsOwnership(ctx context.Context, conn *pgx.Conn, newOwner string) error {
	sanitizedOwner := pgx.Identifier{newOwner}.Sanitize()

	transferKind := func(kind, alterVerb, listQuery string) error {
		names, err := querySingleColumn(ctx, conn, listQuery, kind)
		if err != nil {
			return err
		}
		for _, name := range names {
			query := fmt.Sprintf("ALTER %s %s OWNER TO %s",
				alterVerb, pgx.Identifier{name}.Sanitize(), sanitizedOwner)
			if _, err := conn.Exec(ctx, query); err != nil {
				return fmt.Errorf("alter %s %s owner: %w", kind, name, err)
			}
		}
		return nil
	}

	if err := transferKind("table", "TABLE", `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tableowner = 'postgres'
	`); err != nil {
		return err
	}

	if err := transferKind("sequence", "SEQUENCE", `
		SELECT sequencename FROM pg_sequences
		WHERE schemaname = 'public' AND sequenceowner = 'postgres'
	`); err != nil {
		return err
	}

	if err := transferKind("view", "VIEW", `
		SELECT viewname FROM pg_views
		WHERE schemaname = 'public' AND viewowner = 'postgres'
	`); err != nil {
		return err
	}

	// Functions need their argument signature in the ALTER statement, so they
	// can't go through transferKind.
	if err := transferFunctionOwnership(ctx, conn, sanitizedOwner); err != nil {
		return err
	}

	// Transfer types (custom types, enums, etc.)
	if err := transferKind("type", "TYPE", `
		SELECT t.typname
		FROM pg_type t
		JOIN pg_namespace n ON t.typnamespace = n.oid
		JOIN pg_roles r ON t.typowner = r.oid
		WHERE n.nspname = 'public'
		AND r.rolname = 'postgres'
		AND t.typtype IN ('e', 'c')  -- enums and composite types only
	`); err != nil {
		return err
	}

	return nil
}

// transferFunctionOwnership transfers ownership of public-schema functions and
// procedures owned by postgres.
func transferFunctionOwnership(ctx context.Context, conn *pgx.Conn, sanitizedOwner string) error {
	rows, err := conn.Query(ctx, `
		SELECT p.proname, pg_get_function_identity_arguments(p.oid) as args
		FROM pg_proc p
		JOIN pg_namespace n ON p.pronamespace = n.oid
		JOIN pg_roles r ON p.proowner = r.oid
		WHERE n.nspname = 'public' AND r.rolname = 'postgres'
	`)
	if err != nil {
		return fmt.Errorf("query functions: %w", err)
	}
	type funcSig struct{ name, args string }
	var funcs []funcSig
	for rows.Next() {
		var f funcSig
		if err := rows.Scan(&f.name, &f.args); err != nil {
			rows.Close()
			return fmt.Errorf("scan function name: %w", err)
		}
		funcs = append(funcs, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate functions: %w", err)
	}

	for _, f := range funcs {
		query := fmt.Sprintf("ALTER FUNCTION %s(%s) OWNER TO %s",
			pgx.Identifier{f.name}.Sanitize(), f.args, sanitizedOwner)
		if _, err := conn.Exec(ctx, query); err != nil {
			return fmt.Errorf("alter function %s owner: %w", f.name, err)
		}
	}
	return nil
}

// querySingleColumn collects a single-column string result set. It fully
// drains and closes the rows before returning so the caller can safely Exec
// on the same connection afterwards (pgx forbids Exec while rows are open).
func querySingleColumn(ctx context.Context, conn *pgx.Conn, query, kind string) ([]string, error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query %ss: %w", kind, err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan %s name: %w", kind, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %ss: %w", kind, err)
	}
	return names, nil
}
