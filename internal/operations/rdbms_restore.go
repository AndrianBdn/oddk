package operations

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/api/types/mount"

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
	backupPath, sourceDesc, wantSHA256, err := resolveBackupSource(deps, params)
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
	if err := compressor.ExtractTarZstdWith(ctx, backupPath, tempDir,
		compression.ExtractOptions{WantSHA256: wantSHA256}); err != nil {
		return nil, classifyExtractError("extract backup", err)
	}

	// 8-10. Create the target database and replay the dump into it.
	missingRoles, err := restoreDatabaseFromDumpTree(ctx, deps, conn, dumpRestore{
		instance: instance,
		password: password,
		dumpTree: tempDir,
		sourceDB: params.DatabaseName,
		targetDB: targetDB,
		noun:     "backup",
	})
	if err != nil {
		return nil, err
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

// dumpRestore describes replaying one database out of a per-instance dump tree
// into a live instance.
type dumpRestore struct {
	instance *instances.RDBMSInstance // the TARGET instance
	password string                   // its postgres password

	// dumpTree is a per-instance archive's layout: databases/<db>/ (a
	// directory-format pg_dump) and databases.json. A backup archive is one; so
	// is a logical snapshot's instances/<name>/ subtree (the same
	// stageInstanceDump writes both); and a physical snapshot's single-database
	// restore builds one out of a scratch copy of the cluster.
	dumpTree string
	sourceDB string
	targetDB string

	// noun names the archive kind in the "not found in <noun>" refusal.
	noun string
}

// restoreDatabaseFromDumpTree creates the (empty) target database with the
// source's recorded encoding/collation, pg_restores the dump into it, and
// replays its database-level CREATE grants. Objects arrive owned by postgres
// and without privileges (--no-owner --no-privileges): the target cluster may
// not have the source's roles. Any failure after the CREATE drops the new
// database again, so a refused restore leaves nothing behind.
//
// It returns the grantee roles skipped because they do not exist on the
// target. The caller has already proved the target database is absent.
func restoreDatabaseFromDumpTree(ctx context.Context, deps *Dependencies, conn *pgx.Conn, r dumpRestore) ([]string, error) {
	dbDir := filepath.Join(r.dumpTree, "databases", r.sourceDB)
	if _, err := os.Stat(dbDir); os.IsNotExist(err) {
		return nil, operr.NotFoundf("database %s not found in %s; available databases: %v",
			r.sourceDB, r.noun, listDatabasesInBackup(r.dumpTree))
	}

	createQuery, err := buildRestoreCreateSQL(r.dumpTree, r.sourceDB, r.targetDB)
	if err != nil {
		return nil, err
	}
	createGrantees, err := readDatabaseCreateGrantees(r.dumpTree, r.sourceDB)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, createQuery); err != nil {
		return nil, fmt.Errorf("create target database: %w", err)
	}
	dropTarget := func() {
		dropQuery := fmt.Sprintf("DROP DATABASE IF EXISTS %s", pgx.Identifier{r.targetDB}.Sanitize())
		_, _ = conn.Exec(ctx, dropQuery)
	}

	if err := runPgRestore(ctx, deps, r.instance, r.password, dbDir, r.targetDB); err != nil {
		dropTarget()
		return nil, fmt.Errorf("pg_restore failed: %w", err)
	}

	// pg_restore intentionally skips ACLs because the source's roles may not
	// exist on the target. Replay the database-level CREATE grants captured
	// separately; missing roles are reported and skipped, while a real grant
	// failure removes the new database just like a pg_restore failure does.
	missingRoles, err := restoreDatabaseCreateGrants(ctx, conn, r.targetDB, createGrantees)
	if err != nil {
		dropTarget()
		return nil, fmt.Errorf("restore database CREATE privileges: %w", err)
	}
	return missingRoles, nil
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
// archive path on disk, a human-readable source description, and — for a
// catalogued backup — the SHA-256 recorded when it was written, which the
// extraction checks the file against ("" when there is nothing to check).
func resolveBackupSource(deps *Dependencies, params *RestoreRDBMSParams) (backupPath, sourceDesc, wantSHA256 string, err error) {
	if params.BackupID != 0 {
		backup, err := deps.Store.Backup.GetBackupByID(params.BackupID)
		if err != nil {
			return "", "", "", fmt.Errorf("get backup: %w", err)
		}
		if !backup.LocalLocation.Valid || backup.LocalLocation.String == "" {
			return "", "", "", fmt.Errorf("backup %d has no local copy (download it first)", params.BackupID)
		}
		backupPath = backup.LocalLocation.String
		if !filepath.IsAbs(backupPath) {
			backupPath = filepath.Join(params.BackupDir, backupPath)
		}
		return backupPath, fmt.Sprintf("backup ID %d", params.BackupID), backup.SHA256Str, nil
	}

	if _, err := os.Stat(params.FilePath); err != nil {
		return "", "", "", fmt.Errorf("backup file not found: %s", params.FilePath)
	}
	return params.FilePath, fmt.Sprintf("file %s", filepath.Base(params.FilePath)), "", nil
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
			"-Fd",                                    // Directory format
			"--dbname=" + pgConninfoDBName(targetDB), // Target database, never parsed as a conninfo
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
		Env:  pgRestoreEnv(majorOrZero(instance.Version)),
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

// pgRestoreEnv is the environment of every pg_restore ODDK runs. On
// PostgreSQL 17+ it turns event triggers off for the restore's own sessions.
//
// pg_dump puts event triggers in post-data, so a restore RE-CREATES a login
// trigger near its end — and parallel pg_restore reconnects its main session
// after the worker phase, which is a login to the database it just restored.
// Measured: a login trigger that deletes rows emptied a database that had just
// been restored with every row. A harmless audit trigger fires the same way,
// logging the restore as a user login. event_triggers only exists (and login
// triggers only exist) from 17; older servers refuse the unknown setting, so it
// is not sent to them. pg_restore connects as postgres, which may set it.
func pgRestoreEnv(major int) []string {
	if major >= 17 {
		return []string{"PGOPTIONS=-c event_triggers=off"}
	}
	return nil
}

// majorOrZero parses a PostgreSQL version's major, or 0 when it cannot —
// which gates version-dependent options OFF, the safe direction for a setting
// an older server would refuse.
func majorOrZero(version string) int {
	major, _ := parseMajorVersion(version)
	return major
}
