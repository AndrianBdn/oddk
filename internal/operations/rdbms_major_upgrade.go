package operations

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/mount"
	"github.com/jackc/pgx/v5"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store/instances"
	"github.com/andrianbdn/oddk/internal/store/parameters"
	"github.com/andrianbdn/oddk/internal/util"
)

// UpgradeRDBMSOp performs a major-version PostgreSQL upgrade of an instance
// using a logical dump/restore: it takes a verified full backup, destroys the
// old cluster volume, creates a fresh cluster at the target version (which
// automatically picks the correct data-dir mount for the major), then replays
// globals and every database into it.
//
// A newer PostgreSQL server cannot start against an older major's data
// directory, so an in-place restart is impossible; dump/restore is the only
// universal path that also works with custom images (pgvector, postgis) where
// pg_upgrade would need both majors' binaries in one image.
type UpgradeRDBMSOp struct {
	deps   *Dependencies
	params UpgradeRDBMSParams
	result *UpgradeRDBMSResult
}

// NewUpgradeRDBMSOp creates a new major-upgrade operation
func NewUpgradeRDBMSOp(deps *Dependencies, params UpgradeRDBMSParams) *UpgradeRDBMSOp {
	return &UpgradeRDBMSOp{
		deps:   deps,
		params: params,
	}
}

func (op *UpgradeRDBMSOp) Name() string {
	return fmt.Sprintf("UpgradeRDBMS[%s->%s]", op.params.Name, op.params.TargetVersion)
}

func (op *UpgradeRDBMSOp) Type() OpType {
	return OpTypeWrite
}

// upgradePlan carries everything the destructive stages need, fully resolved
// and validated while the instance is still untouched.
type upgradePlan struct {
	instance       *instances.RDBMSInstance
	targetVersion  string
	targetImage    string
	password       string
	parameterGroup *parameters.ParameterGroup
	roleNames      []string
}

func (op *UpgradeRDBMSOp) Execute(ctx context.Context) error {
	// Validation + planning. The instance is left "running" on any failure
	// up to and including backup verification.
	plan, err := op.validateAndPlan(ctx)
	if err != nil {
		return err
	}

	// Full backup + verification while the instance is still running (the
	// backup itself is not downtime). This is the rollback artifact.
	backupResult, extractedDir, dbs, err := op.takeVerifiedBackup(ctx, plan)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(extractedDir) }()

	// recoverHint is appended to errors after the point of no return so the
	// operator knows the data still lives in the recorded backup.
	recoverHint := fmt.Sprintf("the pre-upgrade backup (ID %d, %s) is intact — recreate the instance at version %s and restore from it",
		backupResult.BackupID, backupResult.BackupPath, plan.instance.Version)

	if err := op.replaceCluster(ctx, plan, recoverHint); err != nil {
		return err
	}

	restored, err := op.restoreIntoCluster(ctx, plan, extractedDir, dbs, recoverHint)
	if err != nil {
		return err
	}

	return op.finalize(plan, backupResult, restored)
}

// markError records "error" status — used by every failure past the first
// destructive step (the instance can no longer be left "running" honestly).
func (op *UpgradeRDBMSOp) markError() {
	if statusErr := op.deps.Store.Instances.UpdateStatus(op.params.Name, "error"); statusErr != nil {
		log.Printf("Error updating status to error: %v", statusErr)
	}
}

// validateAndPlan checks every precondition and resolves everything the
// upgrade needs (target image, password, parameter group, expected roles)
// without touching the instance.
func (op *UpgradeRDBMSOp) validateAndPlan(ctx context.Context) (*upgradePlan, error) {
	name := op.params.Name

	instance, err := op.deps.Store.Instances.Get(name)
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}

	if instance.Status != "running" {
		return nil, operr.Invalidf("instance is not running (status: %s); start it before upgrading", instance.Status)
	}

	if op.params.TargetVersion == "" {
		return nil, operr.Invalidf("target version is required")
	}
	targetMajor, ok := parseMajorVersion(op.params.TargetVersion)
	if !ok {
		return nil, operr.Invalidf("invalid target version: %s", op.params.TargetVersion)
	}
	currentMajor, ok := parseMajorVersion(instance.Version)
	if !ok {
		return nil, fmt.Errorf("cannot parse current version %q", instance.Version)
	}
	if targetMajor <= currentMajor {
		return nil, operr.Invalidf("target version %d must be greater than current version %d (use 'oddk instance switch' for same-major image changes or minor bumps)", targetMajor, currentMajor)
	}

	targetVersion := strconv.Itoa(targetMajor)
	targetImage, err := resolveTargetImage(instance.Image, targetVersion, op.params.Image)
	if err != nil {
		return nil, err
	}

	// Target image must already be pulled (mirror switch wording for status mapping)
	if _, exists := op.deps.Docker.CheckImageExists(targetImage); !exists {
		return nil, operr.Invalidf("image not found locally. Please run 'oddk pull --image %s' first", targetImage)
	}

	password, err := crypto.DecryptPassword(instance.Password, op.deps.MasterKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt password: %w", err)
	}
	parameterGroup, err := op.deps.Store.Parameters.GetGroup(instance.ParameterGroup)
	if err != nil {
		return nil, fmt.Errorf("get parameter group %s: %w", instance.ParameterGroup, err)
	}

	// Capture role names from the live instance (status still "running" so
	// ConnectToRunningInstance works) so we can verify globals fully apply
	// after the upgrade. Per-database metadata (owner/encoding/collation)
	// comes from the backup archive itself (takeVerifiedBackup).
	roleNames, err := captureRoleNames(ctx, op.deps, name)
	if err != nil {
		return nil, fmt.Errorf("capture roles: %w", err)
	}

	return &upgradePlan{
		instance:       instance,
		targetVersion:  targetVersion,
		targetImage:    targetImage,
		password:       password,
		parameterGroup: parameterGroup,
		roleNames:      roleNames,
	}, nil
}

// takeVerifiedBackup takes a full backup of the still-running instance,
// extracts it, and verifies it is complete and restorable (globals present,
// per-database metadata present, only libc-locale databases) — all before any
// destructive step. On success the caller owns extractedDir and must remove
// it; on error it is already cleaned up.
func (op *UpgradeRDBMSOp) takeVerifiedBackup(ctx context.Context, plan *upgradePlan) (*BackupRDBMSResult, string, []DatabaseMeta, error) {
	backupResult, err := BackupRDBMS(ctx, op.deps, &BackupRDBMSParams{
		Name:      op.params.Name,
		BackupDir: op.deps.BackupDir,
		Comment:   fmt.Sprintf("pre-major-upgrade %s->%s", plan.instance.Version, plan.targetVersion),
	})
	if err != nil {
		return nil, "", nil, fmt.Errorf("pre-upgrade backup: %w", err)
	}
	log.Printf("Pre-upgrade backup recorded: ID=%d path=%s", backupResult.BackupID, backupResult.BackupPath)

	extractedDir, err := os.MkdirTemp(op.deps.BackupDir, ".upgrade-*")
	if err != nil {
		return nil, "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	// fail cleans up the temp dir before returning the verification error.
	fail := func(err error) (*BackupRDBMSResult, string, []DatabaseMeta, error) {
		_ = os.RemoveAll(extractedDir)
		return nil, "", nil, err
	}

	if err := compression.NewCompressor().ExtractTarZstd(ctx, backupResult.BackupPath, extractedDir); err != nil {
		return fail(fmt.Errorf("verify backup (extract): %w", err))
	}
	if _, err := os.Stat(filepath.Join(extractedDir, "globals.sql")); err != nil {
		return fail(fmt.Errorf("verify backup: globals.sql missing from archive: %w", err))
	}

	// Read the per-database metadata the backup recorded (owner, encoding,
	// collation, locale provider). Every backup this build produces includes it,
	// so its absence means an incompatible/old archive — refuse rather than
	// guess.
	dbs, found, err := readDatabaseMetadata(extractedDir)
	if err != nil {
		return fail(fmt.Errorf("read database metadata: %w", err))
	}
	if !found {
		return fail(fmt.Errorf("backup archive is missing %s (created by an older ODDK?); cannot major-upgrade from it", databaseMetadataFile))
	}
	if len(dbs) == 0 {
		return fail(fmt.Errorf("verify backup: archive contains no databases"))
	}

	// Guard: automated upgrade can only faithfully reproduce libc-locale
	// databases. Refuse — before any destructive step — if a user database uses
	// a different locale provider (ICU/builtin), rather than silently recreating
	// it with the wrong collation.
	for _, db := range dbs {
		if db.Name == "postgres" {
			continue
		}
		if db.LocProvider != "c" {
			return fail(operr.Invalidf("unsupported locale provider %q on database %q: automated major-upgrade only supports libc-locale databases; migrate it manually", db.LocProvider, db.Name))
		}
	}

	return backupResult, extractedDir, dbs, nil
}

// replaceCluster tears down the old container and volume and brings up a
// fresh, empty cluster at the target version. Removing the old volume is the
// FIRST destructive step — from there on the verified backup is the only copy
// of the data, so every failure marks the instance "error" and carries
// recoverHint.
func (op *UpgradeRDBMSOp) replaceCluster(ctx context.Context, plan *upgradePlan, recoverHint string) error {
	name := op.params.Name
	instance := plan.instance

	// Mark the instance as upgrading. From here, DB access uses direct
	// connections (ConnectToRunningInstance rejects non-"running" status).
	if err := op.deps.Store.Instances.UpdateStatus(name, "upgrading"); err != nil {
		log.Printf("Error updating status to upgrading: %v", err)
	}

	// Stop + remove the old container.
	if instance.ContainerID != "" {
		if status, sErr := op.deps.Docker.GetContainerStatus(instance.ContainerID); sErr == nil && status == "running" {
			if err := op.deps.Docker.StopContainer(instance.ContainerID); err != nil {
				op.markError()
				return fmt.Errorf("stop old container: %w", err)
			}
		}
		if err := op.deps.Docker.RemoveContainer(instance.ContainerID); err != nil {
			op.markError()
			return fmt.Errorf("remove old container: %w", err)
		}
	}

	// Remove the old volume.
	volumeName := fmt.Sprintf("oddk-data-%s", name)
	if err := op.deps.Docker.RemoveVolume(volumeName); err != nil {
		op.markError()
		return fmt.Errorf("remove old volume: %w; %s", err, recoverHint)
	}

	// Create a fresh cluster at the target version. CreateContainer picks
	// the correct data-dir mount target for the major automatically.
	newContainerID, err := op.deps.Docker.CreateContainer(
		name,
		plan.targetVersion,
		plan.targetImage,
		instance.Port,
		plan.password,
		instance.CPUCores,
		instance.RAMMB,
		instance.ParameterGroup,
		plan.parameterGroup.Parameters,
	)
	if err != nil {
		op.markError()
		return fmt.Errorf("create target cluster: %w; %s", err, recoverHint)
	}
	if err := op.deps.Store.Instances.UpdateContainerID(name, newContainerID); err != nil {
		log.Printf("Error updating container ID: %v", err)
	}

	// Start and wait for the fresh cluster to accept connections (initdb
	// runs on first start of the new volume).
	if err := op.deps.Docker.StartContainer(newContainerID); err != nil {
		op.markError()
		return fmt.Errorf("start target cluster: %w; %s", err, recoverHint)
	}
	if err := waitForPostgresReady(ctx, instance.Port, plan.password); err != nil {
		op.markError()
		return fmt.Errorf("target cluster did not become ready: %w; %s", err, recoverHint)
	}
	return nil
}

// restoreIntoCluster replays the extracted backup into the fresh cluster via
// the shared RestoreClusterFromArchive engine, adding the upgrade-specific
// bookkeeping: any failure past this point marks the instance "error" and
// carries recoverHint, since the pre-upgrade backup is now the only copy.
func (op *UpgradeRDBMSOp) restoreIntoCluster(ctx context.Context, plan *upgradePlan, extractedDir string, dbs []DatabaseMeta, recoverHint string) (int, error) {
	restored, err := RestoreClusterFromArchive(ctx, op.deps, RestoreClusterParams{
		InstanceName:  op.params.Name,
		Image:         plan.targetImage,
		Port:          plan.instance.Port,
		Password:      plan.password,
		CPUCores:      plan.instance.CPUCores,
		ExtractedDir:  extractedDir,
		Databases:     dbs,
		ExpectedRoles: plan.roleNames,
	})
	if err != nil {
		op.markError()
		return 0, fmt.Errorf("%w; %s", err, recoverHint)
	}
	return restored, nil
}

// finalize persists the new version/image, marks the instance running, and
// assembles the operation result.
func (op *UpgradeRDBMSOp) finalize(plan *upgradePlan, backupResult *BackupRDBMSResult, restored int) error {
	name := op.params.Name

	if err := op.deps.Store.Instances.UpdateImage(name, plan.targetImage, plan.targetVersion); err != nil {
		op.markError()
		return fmt.Errorf("update instance image/version: %w", err)
	}
	if err := op.deps.Store.Instances.UpdateStatus(name, "running"); err != nil {
		log.Printf("Error updating status to running: %v", err)
	}

	updated, err := op.deps.Store.Instances.Get(name)
	if err != nil {
		return fmt.Errorf("get updated instance: %w", err)
	}

	op.result = &UpgradeRDBMSResult{
		Instance:          updated,
		FromVersion:       plan.instance.Version,
		ToVersion:         plan.targetVersion,
		BackupID:          backupResult.BackupID,
		DatabasesRestored: restored,
	}
	return nil
}

// GetResult returns the operation result
func (op *UpgradeRDBMSOp) GetResult() *UpgradeRDBMSResult {
	return op.result
}

// resolveTargetImage determines the image to upgrade to. For official postgres
// images it defaults to postgres:<targetVersion>; for custom images the caller
// must pass an explicit image (we can't synthesize the tag). When an image is
// given, its detected major must match the target version.
func resolveTargetImage(currentImage, targetVersion, providedImage string) (string, error) {
	if providedImage != "" {
		if detected, ok := docker.DetectPGVersionFromImage(providedImage); ok && detected != targetVersion {
			return "", operr.Invalidf("image tag suggests PostgreSQL %s but --target-version %s was specified", detected, targetVersion)
		}
		return providedImage, nil
	}
	if currentImage == "" || strings.HasPrefix(currentImage, "postgres:") {
		return "postgres:" + targetVersion, nil
	}
	return "", operr.Invalidf("instance uses custom image %s; specify --image for the target version (e.g. --image <repo>:<tag> for PostgreSQL %s)", currentImage, targetVersion)
}

// connectDirect opens a PostgreSQL connection by port+password without checking
// instance status (used during an upgrade when status is "upgrading").
func connectDirect(ctx context.Context, port int, password, database string) (*pgx.Conn, error) {
	connStr := fmt.Sprintf("postgres://postgres:%s@%s:%d/%s?sslmode=disable", password, util.GatewayIP, port, database)
	return pgx.Connect(ctx, connStr)
}

// waitForPostgresReady polls until the cluster accepts connections or times out.
func waitForPostgresReady(ctx context.Context, port int, password string) error {
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := TestPostgreSQLConnectivityWithPassword(ctx, port, password); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("not ready within 90s: %w", lastErr)
}

// listUserDatabasesDirect returns the set of non-template databases on the
// instance using a direct connection.
func listUserDatabasesDirect(ctx context.Context, port int, password string) (map[string]bool, error) {
	conn, err := connectDirect(ctx, port, password, "postgres")
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx, "SELECT datname FROM pg_database WHERE datistemplate = false")
	if err != nil {
		return nil, fmt.Errorf("query databases: %w", err)
	}
	defer rows.Close()

	set := make(map[string]bool)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		set[n] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return set, nil
}

// captureRoleNames returns the non-builtin role names from the live source
// cluster (excluding the bootstrap postgres role and pinned pg_* roles, which
// always exist on a fresh cluster). These are the roles globals.sql must create.
func captureRoleNames(ctx context.Context, deps *Dependencies, instanceName string) ([]string, error) {
	conn, err := ConnectToRunningInstance(ctx, deps, instanceName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx, `SELECT rolname FROM pg_catalog.pg_roles WHERE NOT starts_with(rolname, 'pg_') AND rolname <> 'postgres' ORDER BY rolname`)
	if err != nil {
		return nil, fmt.Errorf("query pg_roles: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan role row: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// verifyRolesPresent fails if any expected role is missing after globals have
// been applied (globals psql runs with ON_ERROR_STOP=0, so a silently-failed
// role would otherwise go unnoticed).
func verifyRolesPresent(ctx context.Context, port int, password string, expected []string) error {
	if len(expected) == 0 {
		return nil
	}
	conn, err := connectDirect(ctx, port, password, "postgres")
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	for _, role := range expected {
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
			return fmt.Errorf("check role %s: %w", role, err)
		}
		if !exists {
			return fmt.Errorf("role %q was not created", role)
		}
	}
	return nil
}

// restoreGlobals replays the dumped globals (roles, role memberships, role
// passwords) into the fresh cluster. ON_ERROR_STOP=0 tolerates statements that
// collide with initdb defaults (e.g. the bootstrap postgres role).
func restoreGlobals(ctx context.Context, deps *Dependencies, instanceName, image string, port int, password, extractedDir string) error {
	return runHelperContainer(ctx, deps, helperContainerSpec{
		ContainerName: fmt.Sprintf("oddk-upgrade-globals-%s-%d", instanceName, time.Now().UnixNano()),
		Image:         image,
		Cmd: []string{
			"psql",
			"-h", util.GatewayIP,
			"-p", strconv.Itoa(port),
			"-U", "postgres",
			"-d", "postgres",
			"-v", "ON_ERROR_STOP=0",
			"-f", "/restore/globals.sql",
		},
		Password: password,
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: extractedDir, Target: "/restore", ReadOnly: true},
		},
		Tool:               "psql",
		LogOutputOnSuccess: true,
	})
}

// restoreDatabaseWithOwner restores a single database, preserving object
// ownership and privileges (no --no-owner / --no-privileges). Roles must
// already exist (restoreGlobals runs first).
func restoreDatabaseWithOwner(ctx context.Context, deps *Dependencies, instanceName, image string, port int, password, dbDir, dbName string, jobs int) error {
	return runHelperContainer(ctx, deps, helperContainerSpec{
		ContainerName: fmt.Sprintf("oddk-upgrade-restore-%s-%s-%d", instanceName, dbName, time.Now().UnixNano()),
		Image:         image,
		Cmd: []string{
			"pg_restore",
			"-Fd",
			"-d", dbName,
			"-h", util.GatewayIP,
			"-p", strconv.Itoa(port),
			"-U", "postgres",
			"-j", strconv.Itoa(jobs),
			"/backup",
		},
		Password: password,
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: dbDir, Target: "/backup", ReadOnly: true},
		},
		Tool: "pg_restore",
	})
}
