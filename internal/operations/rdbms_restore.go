package operations

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/mount"
	"github.com/jackc/pgx/v5"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store/instances"
	"github.com/andrianbdn/oddk/internal/util"
)

// RestoreRDBMSParams contains parameters for restoring a database from backup
type RestoreRDBMSParams struct {
	// Either BackupID or FilePath must be provided (mutually exclusive)
	BackupID int    // ID from backup_history table
	FilePath string // Direct path to .tar.zst file

	InstanceName string // Target instance to restore to
	DatabaseName string // Database name inside the backup to restore
	RestoreAs    string // Optional: restore under a different name
	BackupDir    string // Backup directory for resolving relative paths
}

// RestoreRDBMSResult contains the result of restoring a database
type RestoreRDBMSResult struct {
	TargetDatabase string `json:"targetDatabase"`
	SourceBackup   string `json:"sourceBackup"`
	Message        string `json:"message"`
}

// RestoreRDBMS restores a single database from a backup archive
func RestoreRDBMS(ctx context.Context, deps *Dependencies, params *RestoreRDBMSParams) (*RestoreRDBMSResult, error) {
	// 1. Validate inputs
	if err := validateRestoreParams(params); err != nil {
		return nil, err
	}

	// 2. Determine backup file path
	backupPath, sourceDesc, err := resolveBackupSource(deps, params)
	if err != nil {
		return nil, err
	}

	// 3. Get instance and verify it's running
	instance, err := deps.Store.Instances.Get(params.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}
	if instance == nil {
		return nil, operr.NotFoundf("instance not found: %s", params.InstanceName)
	}
	if instance.Status != "running" {
		return nil, operr.Invalidf("instance %s is not running (status: %s)", params.InstanceName, instance.Status)
	}

	// 4. Decrypt password for PostgreSQL connections
	password, err := crypto.DecryptPassword(instance.Password, deps.MasterKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt password: %w", err)
	}

	// 5. Determine target database name
	targetDB := params.DatabaseName
	if params.RestoreAs != "" {
		targetDB = params.RestoreAs
	}

	// 6. Check target database does NOT exist
	conn, err := ConnectToRunningInstance(ctx, deps, params.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("connect to instance: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var exists bool
	checkQuery := "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)"
	if err := conn.QueryRow(ctx, checkQuery, targetDB).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check if database exists: %w", err)
	}
	if exists {
		return nil, operr.Conflictf("database %s already exists on instance %s", targetDB, params.InstanceName)
	}
	// 7. Extract backup to temp directory
	tempDir, err := os.MkdirTemp(params.BackupDir, ".restore-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	compressor := compression.NewCompressor()
	if err := compressor.ExtractTarZstd(ctx, backupPath, tempDir); err != nil {
		return nil, fmt.Errorf("extract backup: %w", err)
	}

	// 8. Verify requested database exists in backup
	dbDir := filepath.Join(tempDir, "databases", params.DatabaseName)
	if _, err := os.Stat(dbDir); os.IsNotExist(err) {
		// List available databases for helpful error message
		available := listDatabasesInBackup(tempDir)
		return nil, fmt.Errorf("database %s not found in backup; available databases: %v",
			params.DatabaseName, available)
	}

	// 9. Create the (empty) target database.
	createQuery, err := buildRestoreCreateSQL(tempDir, params.DatabaseName, targetDB)
	if err != nil {
		return nil, err
	}
	createGrantees, err := readDatabaseCreateGrantees(tempDir, params.DatabaseName)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, createQuery); err != nil {
		return nil, fmt.Errorf("create target database: %w", err)
	}

	// 10. Run pg_restore via ephemeral container
	if err := runPgRestore(ctx, deps, instance, password, dbDir, targetDB); err != nil {
		dropQuery := fmt.Sprintf("DROP DATABASE IF EXISTS %s", pgx.Identifier{targetDB}.Sanitize())
		_, _ = conn.Exec(ctx, dropQuery)
		return nil, fmt.Errorf("pg_restore failed: %w", err)
	}

	// pg_restore intentionally skips ACLs because backup roles may not exist on
	// the target. Replay the database-level CREATE grants captured separately;
	// missing roles are reported and skipped, while a real grant failure removes
	// the new database just like a pg_restore failure does.
	missingRoles, err := restoreDatabaseCreateGrants(ctx, conn, targetDB, createGrantees)
	if err != nil {
		dropQuery := fmt.Sprintf("DROP DATABASE IF EXISTS %s", pgx.Identifier{targetDB}.Sanitize())
		_, _ = conn.Exec(ctx, dropQuery)
		return nil, fmt.Errorf("restore database CREATE privileges: %w", err)
	}
	if len(missingRoles) > 0 {
		log.Printf(
			"WARNING: restore: skipped CREATE grants on database %q for roles absent from the target: %s",
			targetDB,
			strings.Join(missingRoles, ", "),
		)
	}

	return &RestoreRDBMSResult{
		TargetDatabase: targetDB,
		SourceBackup:   sourceDesc,
		Message:        fmt.Sprintf("Successfully restored database %s from %s", targetDB, sourceDesc),
	}, nil
}

// validateRestoreParams rejects missing/conflicting inputs and path-unsafe
// database names. DatabaseName is joined into a filesystem path
// (databases/<name>) and RestoreAs becomes the target DB; both must be
// portable before either touches the filesystem.
func validateRestoreParams(params *RestoreRDBMSParams) error {
	if params.BackupID == 0 && params.FilePath == "" {
		return fmt.Errorf("either backup ID or file path must be provided")
	}
	if params.BackupID != 0 && params.FilePath != "" {
		return fmt.Errorf("backup ID and file path are mutually exclusive")
	}
	if params.InstanceName == "" {
		return fmt.Errorf("instance name is required")
	}
	if params.DatabaseName == "" {
		return fmt.Errorf("database name is required")
	}
	if err := validatePortableDBName(params.DatabaseName); err != nil {
		return err
	}
	if params.RestoreAs != "" {
		if err := validatePortableDBName(params.RestoreAs); err != nil {
			return err
		}
	}
	return nil
}

// resolveBackupSource maps the restore input (backup ID or file path) to the
// archive path on disk plus a human-readable source description.
func resolveBackupSource(deps *Dependencies, params *RestoreRDBMSParams) (backupPath, sourceDesc string, err error) {
	if params.BackupID != 0 {
		backup, err := deps.Store.Backup.GetBackupByID(params.BackupID)
		if err != nil {
			return "", "", fmt.Errorf("get backup: %w", err)
		}
		if !backup.LocalLocation.Valid || backup.LocalLocation.String == "" {
			return "", "", fmt.Errorf("backup %d has no local copy (download it first)", params.BackupID)
		}
		backupPath = backup.LocalLocation.String
		if !filepath.IsAbs(backupPath) {
			backupPath = filepath.Join(params.BackupDir, backupPath)
		}
		return backupPath, fmt.Sprintf("backup ID %d", params.BackupID), nil
	}

	if _, err := os.Stat(params.FilePath); err != nil {
		return "", "", fmt.Errorf("backup file not found: %s", params.FilePath)
	}
	return params.FilePath, fmt.Sprintf("file %s", filepath.Base(params.FilePath)), nil
}

// buildRestoreCreateSQL builds the CREATE DATABASE statement for the restore
// target. It reproduces the source database's encoding/collation when the
// backup recorded it (databases.json); older archives without it, and
// non-libc-locale databases, fall back to a bare create with the cluster
// defaults.
func buildRestoreCreateSQL(extractedDir, sourceDBName, targetDB string) (string, error) {
	createQuery := fmt.Sprintf("CREATE DATABASE %s", pgx.Identifier{targetDB}.Sanitize())
	metas, found, err := readDatabaseMetadata(extractedDir)
	if err != nil {
		return "", fmt.Errorf("read database metadata: %w", err)
	}
	if found {
		for _, m := range metas {
			if m.Name != sourceDBName {
				continue
			}
			if m.LocProvider == "c" {
				createQuery = buildCreateDatabaseSQL(targetDB, m, false)
			} else {
				log.Printf("restore: database %q uses locale provider %q; recreating %q with cluster defaults (locale not preserved)", m.Name, m.LocProvider, targetDB)
			}
			break
		}
	}
	return createQuery, nil
}

// readDatabaseCreateGrantees returns the source database's recorded CREATE
// grantees. Older archives and older databases.json files have no such data,
// in which case restore retains its historical behavior and returns no roles.
func readDatabaseCreateGrantees(extractedDir, sourceDBName string) ([]string, error) {
	metas, found, err := readDatabaseMetadata(extractedDir)
	if err != nil {
		return nil, fmt.Errorf("read database metadata: %w", err)
	}
	if !found {
		return nil, nil
	}
	for _, meta := range metas {
		if meta.Name == sourceDBName {
			return meta.CreateGrantees, nil
		}
	}
	return nil, nil
}

// listDatabasesInBackup returns names of databases available in the extracted backup
func listDatabasesInBackup(extractedDir string) []string {
	dbsDir := filepath.Join(extractedDir, "databases")
	entries, err := os.ReadDir(dbsDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

// runPgRestore executes pg_restore in an ephemeral container
func runPgRestore(ctx context.Context, deps *Dependencies, instance *instances.RDBMSInstance, password, dbDir, targetDB string) error {
	image := instance.Image
	if image == "" {
		image = fmt.Sprintf("postgres:%s", instance.Version)
	}

	return runHelperContainer(ctx, deps, helperContainerSpec{
		ContainerName: fmt.Sprintf("oddk-restore-%s-%d", instance.Name, time.Now().Unix()),
		Image:         image,
		Cmd: []string{
			"pg_restore",
			"-Fd",          // Directory format
			"-d", targetDB, // Target database
			"-h", util.GatewayIP,
			"-p", fmt.Sprintf("%d", instance.Port),
			"-U", "postgres",
			"--no-owner",      // Skip ownership
			"--no-privileges", // Skip privileges
			"-j", "4",         // Parallel jobs
			"/backup", // Mount point
		},
		Password: password,
		Mounts: []mount.Mount{
			{
				Type:     mount.TypeBind,
				Source:   dbDir,
				Target:   "/backup",
				ReadOnly: true,
			},
		},
		Tool: "pg_restore",
	})
}

// restoreDatabaseCreateGrants reapplies captured CREATE privileges to roles
// present on the target. Missing roles are returned to the caller rather than
// failing an otherwise successful data restore.
func restoreDatabaseCreateGrants(
	ctx context.Context,
	conn *pgx.Conn,
	databaseName string,
	grantees []string,
) ([]string, error) {
	if len(grantees) == 0 {
		return nil, nil
	}
	rows, err := conn.Query(
		ctx,
		"SELECT rolname FROM pg_catalog.pg_roles WHERE rolname = ANY($1) ORDER BY rolname",
		grantees,
	)
	if err != nil {
		return nil, fmt.Errorf("find target roles: %w", err)
	}
	existing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read target roles: %w", err)
	}

	statements, missing := buildDatabaseCreateGrantSQL(databaseName, grantees, existing)
	for _, grantSQL := range statements {
		if _, err := conn.Exec(ctx, grantSQL); err != nil {
			return nil, fmt.Errorf("apply %s: %w", grantSQL, err)
		}
	}
	return missing, nil
}

func buildDatabaseCreateGrantSQL(databaseName string, grantees, existingRoles []string) ([]string, []string) {
	existing := make(map[string]struct{}, len(existingRoles))
	for _, role := range existingRoles {
		existing[role] = struct{}{}
	}
	seen := make(map[string]struct{}, len(grantees))
	statements := make([]string, 0, len(grantees))
	missing := make([]string, 0)
	for _, role := range grantees {
		if _, duplicate := seen[role]; duplicate {
			continue
		}
		seen[role] = struct{}{}
		if _, ok := existing[role]; ok {
			statements = append(statements, fmt.Sprintf(
				"GRANT CREATE ON DATABASE %s TO %s",
				pgx.Identifier{databaseName}.Sanitize(),
				pgx.Identifier{role}.Sanitize(),
			))
		} else {
			missing = append(missing, role)
		}
	}
	return statements, missing
}
