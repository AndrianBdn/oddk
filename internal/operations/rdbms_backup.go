package operations

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/mount"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/rfc3339time"
	"github.com/andrianbdn/oddk/internal/store/backup"
	"github.com/andrianbdn/oddk/internal/store/instances"
	"github.com/andrianbdn/oddk/internal/util"
)

// BackupRDBMSParams contains parameters for backing up an RDBMS instance
type BackupRDBMSParams struct {
	Name      string
	BackupDir string
	Comment   string // Optional comment for the backup
}

// BackupRDBMSResult contains the result of backing up an RDBMS instance
type BackupRDBMSResult struct {
	BackupID   int
	BackupPath string
	Size       int64
	Timestamp  time.Time
}

// BackupRDBMS creates a backup of an RDBMS instance
func BackupRDBMS(ctx context.Context, deps *Dependencies, params *BackupRDBMSParams) (*BackupRDBMSResult, error) {
	instance, err := deps.Store.Instances.Get(params.Name)
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}
	if instance == nil {
		return nil, operr.NotFoundf("instance not found: %s", params.Name)
	}

	// Decrypt password
	password, err := crypto.DecryptPassword(instance.Password, deps.MasterKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt password: %w", err)
	}

	// Check if backup directory exists
	if _, err := os.Stat(params.BackupDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("backup directory does not exist: %s", params.BackupDir)
	}

	timestamp := time.Now().UTC()
	timestampStr := timestamp.Format("20060102150405")
	counter := util.BackupCounter.GetNext(timestampStr)
	backupName := fmt.Sprintf("backup-%s-%s-%d", params.Name, timestampStr, counter)
	backupPath := filepath.Join(params.BackupDir, backupName)
	tempDir := filepath.Join(params.BackupDir, fmt.Sprintf(".tmp-%s", backupName))

	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir) // Clean up temp dir
	}()

	// 1. Stage the full instance dump (globals, databases, metadata).
	if err := stageInstanceDump(ctx, deps, instance, password, tempDir); err != nil {
		return nil, err
	}

	// 2. Create tar archive with zstd compression
	archivePath := backupPath + ".tar.zst"
	size, err := compression.NewCompressor().CreateTarZstd(ctx, tempDir, archivePath)
	if err != nil {
		return nil, fmt.Errorf("create archive: %w", err)
	}

	// 3. Record backup in database
	record := recordBackup(deps, params, archivePath, size, timestamp)

	return &BackupRDBMSResult{
		BackupID:   record.ID,
		BackupPath: archivePath,
		Size:       size,
		Timestamp:  timestamp,
	}, nil
}

// stageInstanceDump writes a complete dump of one instance into dir:
// globals.sql, databases/<db>/, databases.json and instance.json.
//
// This is the shared body of both archive shapes — a per-instance backup
// (where dir becomes the archive root) and one instances/<name>/ subtree of a
// snapshot — so the two can never drift into producing different layouts.
//
// The instance must be running: everything except instance.json is read from
// the live cluster.
func stageInstanceDump(ctx context.Context, deps *Dependencies, instance *instances.RDBMSInstance, password, dir string) error {
	// 1. Backup globals using pg_dumpall
	globalsPath := filepath.Join(dir, "globals.sql")
	if err := backupGlobals(ctx, deps, instance, password, globalsPath); err != nil {
		return fmt.Errorf("backup globals: %w", err)
	}

	// 2. Dump every database (directory-format pg_dump) into dir.
	if err := dumpAllDatabases(ctx, deps, instance, password, dir); err != nil {
		return err
	}

	// 3. Record per-database metadata (owner, encoding, collation, locale
	//    provider) so restore and major-upgrade can recreate databases
	//    faithfully instead of with the target cluster's defaults.
	major, ok := parseMajorVersion(instance.Version)
	if !ok {
		return fmt.Errorf("cannot parse instance version %q", instance.Version)
	}
	metas, err := captureDatabaseMetadata(ctx, deps, instance.Name, major)
	if err != nil {
		return fmt.Errorf("capture database metadata: %w", err)
	}
	if err := writeDatabaseMetadata(dir, metas); err != nil {
		return fmt.Errorf("write database metadata: %w", err)
	}

	// 4. Record the instance's own configuration (version, image, port, CPU/RAM,
	//    parameter group) so the container itself can be rebuilt on another
	//    host, not just the data inside it.
	if err := writeInstanceMetadata(dir, captureInstanceMetadata(deps, instance)); err != nil {
		return fmt.Errorf("write instance metadata: %w", err)
	}

	return nil
}

// dumpAllDatabases lists the instance's databases and dumps each one in
// pg_dump directory format under tempDir/databases/<name>.
func dumpAllDatabases(ctx context.Context, deps *Dependencies, instance *instances.RDBMSInstance, password, tempDir string) error {
	listResult, err := ListDatabases(ctx, deps, ListDatabasesParams{
		InstanceName: instance.Name,
	})
	if err != nil {
		return fmt.Errorf("list databases: %w", err)
	}

	for _, db := range listResult.Databases {
		// Skip template databases (already filtered by ListDatabases but double-check)
		if db.Name == "template0" || db.Name == "template1" {
			continue
		}

		if err := validatePortableDBName(db.Name); err != nil {
			return err
		}

		dbDir := filepath.Join(tempDir, "databases", db.Name)
		if err := os.MkdirAll(dbDir, 0o750); err != nil {
			return fmt.Errorf("create db dir for %s: %w", db.Name, err)
		}

		if err := backupDatabase(ctx, deps, instance, password, db.Name, dbDir); err != nil {
			return fmt.Errorf("backup database %s: %w", db.Name, err)
		}
	}
	return nil
}

// recordBackup stores the backup record; a store failure is logged but does
// not fail the backup (the archive itself is complete on disk).
func recordBackup(deps *Dependencies, params *BackupRDBMSParams, archivePath string, size int64, timestamp time.Time) *backup.BackupRecord {
	record := &backup.BackupRecord{
		InstanceName: params.Name,
		Timestamp:    rfc3339time.Time{Time: timestamp},
		Size:         size,
		LocalPath:    archivePath,
		Status:       "completed",
	}
	if params.Comment != "" {
		record.Comment = sql.NullString{String: params.Comment, Valid: true}
		record.CommentStr = params.Comment
	}
	if err := deps.Store.Backup.RecordBackup(record); err != nil {
		fmt.Printf("Warning: failed to record backup in database: %v\n", err)
	}
	return record
}

func backupGlobals(ctx context.Context, deps *Dependencies, instance *instances.RDBMSInstance, password, outputPath string) error {
	image := instance.Image
	if image == "" {
		image = fmt.Sprintf("postgres:%s", instance.Version)
	}

	// pg_dumpall writes SQL to stdout; capture it and write the file only
	// after the helper has succeeded.
	var buf bytes.Buffer
	err := runHelperContainer(ctx, deps, helperContainerSpec{
		ContainerName: fmt.Sprintf("oddk-backup-globals-%s-%d", instance.Name, time.Now().Unix()),
		Image:         image,
		Cmd: []string{
			"pg_dumpall",
			"-g", // Globals only
			"-h", util.GatewayIP,
			"-p", fmt.Sprintf("%d", instance.Port),
			"-U", "postgres",
		},
		Password:      password,
		Tool:          "pg_dumpall",
		CaptureStdout: &buf,
	})
	if err != nil {
		return err
	}

	file, err := os.Create(outputPath) // #nosec G304 - outputPath is controlled by backup operation
	if err != nil {
		return fmt.Errorf("create globals file: %w", err)
	}
	defer func() { _ = file.Close() }()

	if _, err := file.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write globals file: %w", err)
	}

	return nil
}

func backupDatabase(ctx context.Context, deps *Dependencies, instance *instances.RDBMSInstance, password, dbName, outputDir string) error {
	image := instance.Image
	if image == "" {
		image = fmt.Sprintf("postgres:%s", instance.Version)
	}

	// Verify output directory exists
	if _, err := os.Stat(outputDir); os.IsNotExist(err) {
		return fmt.Errorf("output directory does not exist: %s", outputDir)
	}

	return runHelperContainer(ctx, deps, helperContainerSpec{
		ContainerName: fmt.Sprintf("oddk-backup-db-%s-%s-%d", instance.Name, dbName, time.Now().Unix()),
		Image:         image,
		Cmd: []string{
			"pg_dump",
			"-Fd",     // Directory format
			"-j", "4", // Parallel jobs
			"-Z0", // No compression (we'll use zstd later)
			"-h", util.GatewayIP,
			"-p", fmt.Sprintf("%d", instance.Port),
			"-U", "postgres",
			"--file", "/backup",
			dbName,
		},
		Password: password,
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: outputDir,
				Target: "/backup",
			},
		},
		Tool: "pg_dump",
	})
}

// validatePortableDBName rejects database names that are unsafe to use as a
// path component in the backup archive layout (databases/<name>/...). ODDK
// backups support only names that map cleanly to a single directory; a name
// containing a path separator or a "."/".." segment is refused loudly rather
// than producing a misleading partial or unrestorable backup. Dots, spaces,
// unicode and mixed case are allowed so backups of externally-created databases
// keep working — only path-unsafe names are rejected.
func validatePortableDBName(name string) error {
	if name == "" {
		return fmt.Errorf("empty database name is not supported by ODDK backups")
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') ||
		strings.ContainsRune(name, os.PathSeparator) {
		return fmt.Errorf("database name %q is not supported by ODDK backups (contains a path separator)", name)
	}
	if name == "." || name == ".." || filepath.Clean(name) != name {
		return fmt.Errorf("database name %q is not supported by ODDK backups", name)
	}
	return nil
}

// pgPassMountTarget is where helper containers find the throwaway .pgpass
// credential file (PGPASSFILE points here). Passing the superuser password via a
// mounted 0600 file keeps it out of the container environment, where
// `docker inspect` would otherwise expose it for the helper's lifetime.
const pgPassMountTarget = "/oddk-cred/.pgpass" // #nosec G101 - container filesystem path, not a credential

// newPgPassMount writes a throwaway 0600 .pgpass file in a fresh temp dir under
// baseDir and returns a read-only bind mount exposing it at /oddk-cred plus the
// matching PGPASSFILE env entry. The file is owned by the daemon's uid; helper
// containers run as that uid (or as root), so both can read it, while the 0600
// mode satisfies libpq's permission check. The caller must invoke cleanup once
// the helper container has finished.
func newPgPassMount(baseDir, password string) (m mount.Mount, env string, cleanup func(), err error) {
	dir, err := os.MkdirTemp(baseDir, ".pgpass-*")
	if err != nil {
		return mount.Mount{}, "", func() {}, fmt.Errorf("create pgpass dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	// .pgpass line matches any host/port/db/user; only ':' and '\' in a field
	// need escaping.
	line := "*:*:*:*:" + escapePgPassField(password) + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".pgpass"), []byte(line), 0o600); err != nil {
		cleanup()
		return mount.Mount{}, "", func() {}, fmt.Errorf("write pgpass file: %w", err)
	}
	m = mount.Mount{Type: mount.TypeBind, Source: dir, Target: "/oddk-cred", ReadOnly: true}
	return m, "PGPASSFILE=" + pgPassMountTarget, cleanup, nil
}

// escapePgPassField escapes the backslash and colon characters that are special
// in a .pgpass field.
func escapePgPassField(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `:`, `\:`)
	return s
}
