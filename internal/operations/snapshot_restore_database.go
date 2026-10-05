package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/api/types/mount"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store/instances"
	"github.com/andrianbdn/oddk/internal/store/parameters"
	"github.com/andrianbdn/oddk/internal/util"
	"github.com/andrianbdn/oddk/internal/version"
)

// RestoreDatabaseParams describes restoring ONE database out of a snapshot
// into a live instance — the "someone dropped a table" path, and with
// RestoreAs the "give me yesterday's copy next to today's" path.
//
// It is the snapshot counterpart of `backup restore --database`, and shares its
// core (restoreDatabaseFromDumpTree). Almost every hazard of restore-instance
// is absent by construction: the instance's data volume, its other databases,
// its roles, its postgres password and its configuration are all untouched.
// The one thing written is a database that did not exist before.
type RestoreDatabaseParams struct {
	ArchivePath  string
	InstanceName string // the target instance: the live one the database is created on

	// SourceInstance is the snapshot entry the database is read from. Empty
	// means InstanceName — restoring an instance's own database. Naming
	// another lets prod's database from last night land on staging.
	SourceInstance string
	DatabaseName   string // the database inside the snapshot
	RestoreAs      string // optional: the name to create it under

	// MasterKeyPath is the SOURCE host's master.key. Only a physical entry
	// needs a key at all (to open the scratch copy of the cluster, whose roles
	// are the source's), and only when the snapshot came from elsewhere.
	MasterKeyPath string

	BackupDir string
	Progress  io.Writer

	// ExpectedSHA256 is the catalogue's digest for the archive, when it was
	// resolved through the catalogue (see ArchiveOrigin.ExpectedSHA256).
	ExpectedSHA256 string
}

// RestoreDatabaseResult describes what was restored.
type RestoreDatabaseResult struct {
	Instance       string    `json:"instance"`
	SourceInstance string    `json:"sourceInstance"`
	SourceDatabase string    `json:"sourceDatabase"`
	TargetDatabase string    `json:"targetDatabase"`
	Format         string    `json:"format"`
	SourceHost     string    `json:"sourceHost"`
	SnapshotAt     time.Time `json:"snapshotAt"`

	// SkippedGrantRoles are roles the source granted CREATE on the database
	// that do not exist on the target, so the grant could not be replayed.
	SkippedGrantRoles []string `json:"skippedGrantRoles,omitempty"`

	// Warnings are things the operator must know that did not fail the
	// restore — e.g. a scratch copy of the instance's data that could not be
	// removed.
	Warnings []string `json:"warnings,omitempty"`

	// ArchiveOrigin is set by the caller that resolved the archive source.
	ArchiveOrigin *ArchiveOrigin `json:"archiveOrigin,omitempty"`
}

// scratchReadyTimeout bounds how long a scratch copy may take to recover. It is
// longer than waitForPostgresReady's 90s on purpose: a physical entry replays
// the capture window's WAL before accepting connections, and a restore that
// gives up on a busy capture is a restore that does not work when needed.
const scratchReadyTimeout = 5 * time.Minute

// RestoreDatabaseFromSnapshot restores one database out of a snapshot entry
// into a live instance — the same-named one by default, or any other
// (SourceInstance), e.g. prod's database into staging.
//
// A LOGICAL entry already holds per-database dumps, laid out exactly like a
// backup archive, so it is replayed directly. A PHYSICAL entry holds a copy of
// the whole cluster: it is started as an isolated scratch cluster (no network;
// see docker.CreateScratchCluster), the one database is pg_dumped out of it,
// and that dump is replayed. Either way the target sees the same thing a
// `backup restore` produces: a new database, objects owned by postgres, no
// object privileges, database-level CREATE grants replayed where the role
// exists.
func RestoreDatabaseFromSnapshot(ctx context.Context, deps *Dependencies, params *RestoreDatabaseParams) (_ *RestoreDatabaseResult, err error) {
	// warnings must reach the operator whether or not the restore succeeds: a
	// scratch copy that could not be removed is still on disk after a failure.
	var warnings []string
	defer func() {
		if err != nil && len(warnings) > 0 {
			err = fmt.Errorf("%w (also: %s)", err, strings.Join(warnings, "; "))
		}
	}()
	if err := validateRestoreDatabaseParams(params); err != nil {
		return nil, err
	}
	targetDB := params.DatabaseName
	if params.RestoreAs != "" {
		targetDB = params.RestoreAs
	}
	source := params.SourceInstance
	if source == "" {
		source = params.InstanceName
	}

	// 1. The target must be a live instance: this restores INTO a cluster.
	instance, err := deps.Store.Instances.Get(params.InstanceName)
	if errors.Is(err, operr.ErrNotFound) {
		hint := fmt.Sprintf("'oddk snapshot restore-instance --instance %s' rebuilds the whole instance from the snapshot", source)
		if source != params.InstanceName {
			hint = fmt.Sprintf("create it first with 'oddk create --name %s ...'", params.InstanceName)
		}
		return nil, operr.NotFoundf(
			"instance %q does not exist here. A single-database restore needs a running instance to restore into; %s",
			params.InstanceName, hint)
	}
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}
	if instance.Status != instances.StatusRunning {
		return nil, operr.Invalidf("instance %s is not running (status: %s)", params.InstanceName, instance.Status)
	}
	password, err := crypto.DecryptPassword(instance.Password, deps.MasterKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt password: %w", err)
	}

	// 2. The target database must be absent. Checked before the archive is
	//    touched: it is the cheapest refusal, and the likeliest one.
	conn, err := ConnectToRunningInstance(ctx, deps, params.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("connect to instance: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := refuseExistingDatabase(ctx, conn, params.InstanceName, targetDB); err != nil {
		return nil, err
	}

	// 3. Manifest: compatibility, and whether this instance's data is in here.
	manifest, entry, err := readRestoreManifest(params.ArchivePath, source)
	if err != nil {
		return nil, err
	}
	if !entry.HasData {
		return nil, operr.Invalidf(
			"instance %q was captured configuration-only (%s), so the snapshot holds no databases for it",
			source, entrySkipReason(entry))
	}
	if err := refuseNewerSourceMajor(entry.Version, instance.Version); err != nil {
		return nil, err
	}
	physical := entryFormat(entry) == SnapshotFormatPhysical
	if physical {
		if _, err := ensureImagesPresent(ctx, deps.Docker, []SnapshotInstanceEntry{entry}, params.Progress, nil); err != nil {
			return nil, err
		}
		if err := checkPhysicalImageMajor(deps.Docker, entry); err != nil {
			return nil, err
		}
	}

	// 4. Extract only what this restore reads. The whole archive is still
	//    verified; only this instance's subtree (and, for physical, oddk.db,
	//    which holds the credential for the scratch copy) reaches the disk.
	extractedDir, err := os.MkdirTemp(params.BackupDir, SnapshotStagingPrefix+"restore-db-*")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(extractedDir) }()

	if err := compression.NewCompressor().ExtractTarZstdWith(ctx, params.ArchivePath, extractedDir, compression.ExtractOptions{
		Keep:       restoreDatabaseMembers(source, physical),
		WantSHA256: params.ExpectedSHA256,
	}); err != nil {
		return nil, classifyExtractError("extract snapshot", err)
	}
	emitLine(params.Progress, "  ✓ Snapshot extracted (instance %s only)", source)

	instanceDir := filepath.Join(extractedDir, snapshotInstancesDir, source)
	meta, metaFound, err := readInstanceMetadata(instanceDir)
	if err != nil {
		return nil, err
	}
	if !metaFound {
		return nil, operr.Invalidf("snapshot lists instance %s but the archive has no %s for it",
			source, instanceMetadataFile)
	}
	// meta drives the scratch cluster's image, version and shape; it must be
	// the entry that was asked for (see the same check in restore-instance).
	if meta.Name != source {
		return nil, operr.Invalidf("archive is inconsistent: instances/%s/%s declares the instance name %q",
			source, instanceMetadataFile, meta.Name)
	}

	// 5. Produce a dump tree (databases/<db>/ + databases.json) for the one
	//    database. A logical entry IS one.
	dumpTree := instanceDir
	if physical {
		masterKey := deps.MasterKey
		if params.MasterKeyPath != "" {
			masterKey, err = crypto.ReadKeyFileAt(params.MasterKeyPath)
			if err != nil {
				return nil, operr.Invalidf("read source master key: %v", err)
			}
		}
		sourcePassword, err := snapshotInstancePassword(filepath.Join(extractedDir, snapshotStoreFile), source, masterKey)
		if err != nil {
			return nil, err
		}

		dumpTree = filepath.Join(extractedDir, "dump")
		cleanupWarning, err := dumpDatabaseFromPhysicalEntry(ctx, deps, scratchDump{
			meta:           meta,
			instanceDir:    instanceDir,
			password:       sourcePassword,
			database:       params.DatabaseName,
			dumpTree:       dumpTree,
			parameterGroup: scratchParameters(deps, meta),
			progress:       params.Progress,
		})
		if cleanupWarning != "" {
			warnings = append(warnings, cleanupWarning)
		}
		if err != nil {
			return nil, err
		}
	}

	// 6. Replay it into the live instance.
	missingRoles, err := restoreDatabaseFromDumpTree(ctx, deps, conn, dumpRestore{
		instance: instance,
		password: password,
		dumpTree: dumpTree,
		sourceDB: params.DatabaseName,
		targetDB: targetDB,
		noun:     "snapshot",
	})
	if err != nil {
		return nil, err
	}
	if len(missingRoles) > 0 {
		log.Printf("WARNING: snapshot restore-database: skipped CREATE grants on database %q for roles absent from the target: %s",
			targetDB, strings.Join(missingRoles, ", "))
	}
	if source != params.InstanceName {
		emitLine(params.Progress, "  ✓ Database %s of %s restored into %s as %s", params.DatabaseName, source, params.InstanceName, targetDB)
	} else {
		emitLine(params.Progress, "  ✓ Database %s restored into %s", targetDB, params.InstanceName)
	}

	return &RestoreDatabaseResult{
		Instance:          params.InstanceName,
		SourceInstance:    source,
		SourceDatabase:    params.DatabaseName,
		TargetDatabase:    targetDB,
		Format:            entryFormat(entry),
		SourceHost:        manifest.SourceHost,
		SnapshotAt:        manifest.CreatedAt,
		SkippedGrantRoles: missingRoles,
		Warnings:          warnings,
	}, nil
}

func validateRestoreDatabaseParams(params *RestoreDatabaseParams) error {
	if params.InstanceName == "" {
		return operr.Invalidf("instance name is required")
	}
	if err := util.ValidateInstanceName(params.InstanceName); err != nil {
		return operr.Invalidf("invalid instance name: %v", err)
	}
	if params.SourceInstance != "" {
		// Joined into the extraction filter and a path; validated like any
		// instance name before it touches either.
		if err := util.ValidateInstanceName(params.SourceInstance); err != nil {
			return operr.Invalidf("invalid source instance name: %v", err)
		}
	}
	if params.DatabaseName == "" {
		return operr.Invalidf("database name is required")
	}
	// Both names become path components (databases/<name>) or the target
	// database, so both must be portable before either touches the disk.
	if err := validatePortableDBName(params.DatabaseName); err != nil {
		return operr.Invalidf("%v", err)
	}
	if params.RestoreAs != "" {
		if err := validatePortableDBName(params.RestoreAs); err != nil {
			return operr.Invalidf("%v", err)
		}
	}
	return nil
}

// refuseExistingDatabase refuses to restore over a database that exists.
// Replacing a live database is a different risk class from creating a new one,
// and is not something this command does — hence --restore-as.
func refuseExistingDatabase(ctx context.Context, conn *pgx.Conn, instanceName, targetDB string) error {
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", targetDB).Scan(&exists); err != nil {
		return fmt.Errorf("check if database exists: %w", err)
	}
	if exists {
		return operr.Conflictf(
			"database %s already exists on instance %s; restore it under another name with --restore-as, "+
				"or drop the existing one first if replacing it is really what you want",
			targetDB, instanceName)
	}
	return nil
}

// refuseNewerSourceMajor refuses a dump whose PostgreSQL major is newer than
// the target's. pg_restore reads dumps from older servers, not newer ones, and
// discovering that after a scratch cluster was built and dumped is minutes
// late. Unparseable versions pass: this is a courtesy check, and pg_restore
// remains the authority.
func refuseNewerSourceMajor(sourceVersion, targetVersion string) error {
	source, okSource := parseMajorVersion(sourceVersion)
	target, okTarget := parseMajorVersion(targetVersion)
	if !okSource || !okTarget || source <= target {
		return nil
	}
	return operr.Invalidf(
		"the snapshot's copy of this instance is PostgreSQL %d but the instance is now PostgreSQL %d; "+
			"a database dumped from a newer major cannot be restored into an older one",
		source, target)
}

// readRestoreManifest reads and checks a snapshot's manifest for a restore of
// one of its instances: format and ODDK version compatibility, presence of the
// instance, and — for a physical entry with data — the architecture. The
// manifest is the archive's first member, so all of this costs kilobytes.
//
// Whether a configuration-only entry is acceptable is the caller's call.
func readRestoreManifest(archivePath, instanceName string) (*SnapshotManifest, SnapshotInstanceEntry, error) {
	manifest, err := ReadSnapshotManifestFromArchive(archivePath)
	if err != nil {
		return nil, SnapshotInstanceEntry{}, operr.Invalidf("cannot read snapshot manifest: %v", err)
	}
	if manifest.FormatVersion > SnapshotFormatVersion {
		return nil, SnapshotInstanceEntry{}, operr.Invalidf("snapshot uses archive format v%d but this ODDK understands only v%d; upgrade ODDK",
			manifest.FormatVersion, SnapshotFormatVersion)
	}
	if newer, ok := isVersionNewer(manifest.OddkVersion, version.Version); ok && newer {
		return nil, SnapshotInstanceEntry{}, operr.Invalidf("snapshot was created by oddk %s but this binary is %s; upgrade ODDK to %s or later",
			manifest.OddkVersion, version.Version, manifest.OddkVersion)
	}

	entry, found := snapshotEntry(manifest, instanceName)
	if !found {
		return nil, SnapshotInstanceEntry{}, operr.NotFoundf("snapshot does not contain instance %q (it has: %s)",
			instanceName, instanceNameList(manifest))
	}
	if entryFormat(entry) == SnapshotFormatPhysical && entry.HasData &&
		manifest.SourceArch != "" && manifest.SourceArch != runtime.GOARCH {
		return nil, SnapshotInstanceEntry{}, operr.Invalidf(
			"instance %q was captured physically on %s, but this host is %s; physical clusters are not portable across architectures. Restore on a %s host, or take a --logical snapshot on the source",
			instanceName, manifest.SourceArch, runtime.GOARCH, manifest.SourceArch,
		)
	}
	return manifest, entry, nil
}

// restoreDatabaseMembers selects the archive members a single-database restore
// reads: the manifest, the instance's own subtree, and — for a physical entry
// only — oddk.db, which holds the credential the scratch copy's roles accept.
func restoreDatabaseMembers(instanceName string, physical bool) func(string) bool {
	instancePrefix := path.Join(snapshotInstancesDir, instanceName)
	return func(name string) bool {
		name = strings.TrimPrefix(name, "./")
		name = strings.TrimSuffix(name, "/")
		switch name {
		case snapshotManifestFile, snapshotInstancesDir, instancePrefix:
			return true
		case snapshotStoreFile:
			return physical
		default:
			return strings.HasPrefix(name, instancePrefix+"/")
		}
	}
}

// scratchParameters returns the parameter group the scratch copy runs with: the
// snapshot's inlined definition, else this host's group of that name, else
// none. It never refuses — a scratch copy that runs on defaults is still
// useful, and the only realistic consequence (pg_dump exhausting the lock
// table on a very wide database) fails loudly with PostgreSQL's own hint.
func scratchParameters(deps *Dependencies, meta *InstanceMeta) []parameters.Parameter {
	if meta.ParameterGroupDefinition != nil {
		return meta.ParameterGroupDefinition.Parameters
	}
	if meta.ParameterGroup == "" {
		return nil
	}
	group, err := deps.Store.Parameters.GetGroup(meta.ParameterGroup)
	if err != nil || group == nil {
		log.Printf("snapshot restore-database: parameter group %q is not in the snapshot or on this host; the scratch copy runs with defaults", meta.ParameterGroup)
		return nil
	}
	return group.Parameters
}

// scratchDump is everything needed to dump one database out of a physical
// snapshot entry.
type scratchDump struct {
	meta           *InstanceMeta
	instanceDir    string // the extracted instances/<name> directory
	password       string // the SOURCE's postgres password (the copy's roles are the source's)
	database       string
	dumpTree       string // output: databases/<database>/ + databases.json
	parameterGroup []parameters.Parameter
	progress       io.Writer
}

// dumpDatabaseFromPhysicalEntry brings a physical entry's cluster up as an
// isolated scratch copy, dumps one database out of it into sd.dumpTree, and
// removes the copy — container AND volume — whatever happens.
//
// Disk: the scratch volume holds the whole cluster uncompressed, on top of the
// extracted entry and the dump. That is the price of a physical archive, which
// has no per-database structure to extract from.
//
// cleanupWarning is non-empty when the scratch copy could not be removed. The
// restore itself succeeded, so that is not an error — but it means a full
// copy of the instance's data is still on disk, which the operator must be
// told, not just the daemon log.
func dumpDatabaseFromPhysicalEntry(ctx context.Context, deps *Dependencies, sd scratchDump) (cleanupWarning string, err error) {
	meta := sd.meta
	name := docker.ScratchContainerName(meta.Name, time.Now().UnixNano())
	containerID, err := deps.Docker.CreateScratchCluster(name, meta.Name, meta.Version, meta.Image,
		meta.CPUCores, meta.RAMMB, sd.parameterGroup)
	if err != nil {
		return "", err
	}
	defer func() {
		if rmErr := deps.Docker.RemoveScratchCluster(containerID, name); rmErr != nil {
			cleanupWarning = fmt.Sprintf(
				"the scratch copy of %s could not be removed (%v). It holds a full copy of the instance's data; "+
					"the daemon removes it at its next start, or remove it now with 'docker rm -f %s; docker volume rm %s'",
				meta.Name, rmErr, name, name)
			log.Printf("WARNING: %s", cleanupWarning)
		}
	}()
	err = dumpFromScratch(ctx, deps, sd, containerID)
	return cleanupWarning, err
}

// dumpFromScratch is dumpDatabaseFromPhysicalEntry's body, between creating the
// scratch cluster and removing it.
func dumpFromScratch(ctx context.Context, deps *Dependencies, sd scratchDump, containerID string) error {
	meta := sd.meta

	if err := restorePhysicalIntoCreatedContainer(ctx, deps, containerID, meta.Version, sd.instanceDir); err != nil {
		return err
	}
	if err := deps.Docker.StartContainer(containerID); err != nil {
		return fmt.Errorf("start scratch cluster: %w", err)
	}
	emitLine(sd.progress, "  … Starting an isolated copy of %s from the snapshot (no network)", meta.Name)
	major, ok := parseMajorVersion(meta.Version)
	if !ok {
		return fmt.Errorf("cannot parse instance version %q", meta.Version)
	}
	clientEnv := scratchClientEnv(major)
	if err := waitForScratchReady(ctx, deps, containerID, meta.Image, sd.password, clientEnv); err != nil {
		return err
	}
	emitLine(sd.progress, "  ✓ Snapshot copy of %s recovered", meta.Name)

	metas, err := scratchDatabaseMetadata(ctx, deps, containerID, meta.Image, sd.password, major, clientEnv)
	if err != nil {
		return err
	}
	var found *DatabaseMeta
	available := make([]string, 0, len(metas))
	for i := range metas {
		available = append(available, metas[i].Name)
		if metas[i].Name == sd.database {
			found = &metas[i]
		}
	}
	if found == nil {
		return operr.NotFoundf("database %s not found in snapshot; available databases: %v", sd.database, available)
	}

	dbDir := filepath.Join(sd.dumpTree, "databases", sd.database)
	if err := os.MkdirAll(dbDir, 0o750); err != nil {
		return fmt.Errorf("create dump dir: %w", err)
	}
	if err := writeDatabaseMetadata(sd.dumpTree, []DatabaseMeta{*found}); err != nil {
		return err
	}
	if err := runHelperContainer(ctx, deps, helperContainerSpec{
		ContainerName: fmt.Sprintf("oddk-scratch-dump-%s-%d", meta.Name, time.Now().UnixNano()),
		Image:         meta.Image,
		Cmd: []string{
			"pg_dump", "-Fd", "-j", "4", "-Z0",
			"-h", "127.0.0.1", "-p", "5432", "-U", "postgres",
			"--file", "/backup",
			"--dbname=" + pgConninfoDBName(sd.database),
		},
		Password:           sd.password,
		Mounts:             []mount.Mount{{Type: mount.TypeBind, Source: dbDir, Target: "/backup"}},
		JoinNetNSContainer: containerID,
		Env:                clientEnv,
		Tool:               "pg_dump",
	}); err != nil {
		return fmt.Errorf("dump %s from the snapshot copy: %w", sd.database, err)
	}
	emitLine(sd.progress, "  ✓ Database %s dumped from the snapshot copy", sd.database)
	return nil
}

// scratchClientEnv is the environment of every client that talks to a scratch
// copy: the readiness probe, the metadata query and pg_dump (whose parallel
// connections inherit it, since PGOPTIONS is read by libpq on every connect).
//
// Settings sent by the CLIENT at connect time outrank every server-side
// source, including per-role and per-database settings that ship inside the
// archived cluster — which the server's own -c defaults do NOT. Measured on
// postgres:17: with the server defaulting to read-only, an archived
// `ALTER ROLE postgres SET default_transaction_read_only = off` let a LOGIN
// event trigger delete every row of a table when a client merely connected;
// with these options on the client it did not run.
//   - event_triggers=off (PostgreSQL 17+, which is also when LOGIN triggers
//     appeared): a login trigger fires on every connection, so it needs no
//     background worker and max_worker_processes=0 does not stop it. Even a
//     harmless audit trigger that INSERTs would make every scratch login fail
//     under read-only. Older servers reject an unknown setting outright, and
//     have no login triggers, so it is not sent to them.
//   - default_transaction_read_only=on: the server-side default made binding.
func scratchClientEnv(major int) []string {
	options := "-c default_transaction_read_only=on"
	if major >= 17 {
		options = "-c event_triggers=off " + options
	}
	return []string{"PGOPTIONS=" + options}
}

// waitForScratchReady polls the scratch copy with pg_isready from inside its
// network namespace — it has no other network — until it accepts connections.
// A copy that exits (a recovery failure, a bad parameter) fails immediately
// with its own log rather than after the timeout.
func waitForScratchReady(ctx context.Context, deps *Dependencies, containerID, image, password string, clientEnv []string) error {
	deadline := time.Now().Add(scratchReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := deps.Docker.GetContainerStatus(containerID)
		if err != nil {
			return fmt.Errorf("inspect scratch cluster: %w", err)
		}
		if state != "running" {
			logs := "<logs unavailable>"
			if out, logErr := getContainerLogs(ctx, deps, containerID); logErr == nil {
				logs = out.String()
			}
			return fmt.Errorf("the snapshot copy of the cluster stopped during recovery (state %s): %s", state, logs)
		}
		lastErr = runHelperContainer(ctx, deps, helperContainerSpec{
			ContainerName:      fmt.Sprintf("oddk-scratch-ready-%d", time.Now().UnixNano()),
			Image:              image,
			Cmd:                []string{"pg_isready", "-h", "127.0.0.1", "-p", "5432", "-U", "postgres", "-d", "postgres", "-t", "3"},
			Password:           password,
			JoinNetNSContainer: containerID,
			Env:                clientEnv,
			Tool:               "pg_isready",
		})
		if lastErr == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("the snapshot copy of the cluster did not accept connections within %s: %w", scratchReadyTimeout, lastErr)
}

// scratchDatabaseMetadata reads DatabaseMeta for every database in the scratch
// copy through a netns-joined psql, as one JSON document.
func scratchDatabaseMetadata(ctx context.Context, deps *Dependencies, containerID, image, password string, major int, clientEnv []string) ([]DatabaseMeta, error) {
	var out bytes.Buffer
	if err := runHelperContainer(ctx, deps, helperContainerSpec{
		ContainerName: fmt.Sprintf("oddk-scratch-meta-%d", time.Now().UnixNano()),
		Image:         image,
		Cmd: []string{
			"psql", "-X", "-At", "-v", "ON_ERROR_STOP=1",
			"-h", "127.0.0.1", "-p", "5432", "-U", "postgres", "-d", "postgres",
			"-c", scratchMetadataSQL(major),
		},
		Password:           password,
		JoinNetNSContainer: containerID,
		Env:                clientEnv,
		Tool:               "psql",
		CaptureStdout:      &out,
	}); err != nil {
		return nil, fmt.Errorf("read database metadata from the snapshot copy: %w", err)
	}
	return parseScratchMetadata(out.Bytes())
}

// scratchMetadataSQL aggregates databaseMetadataQuery into a single JSON array
// in DatabaseMeta's own JSON shape, so psql's output needs no column parsing.
func scratchMetadataSQL(major int) string {
	return fmt.Sprintf(`SELECT coalesce(json_agg(json_build_object(
		'name', m.dbname, 'owner', m.dbowner, 'encoding', m.dbencoding,
		'collate', m.dbcollate, 'ctype', m.dbctype, 'locProvider', m.dblocprovider,
		'createGrantees', m.dbcreategrantees)), '[]'::json)
		FROM (%s) AS m`, databaseMetadataQuery(major))
}

func parseScratchMetadata(raw []byte) ([]DatabaseMeta, error) {
	var metas []DatabaseMeta
	if err := json.Unmarshal(bytes.TrimSpace(raw), &metas); err != nil {
		return nil, fmt.Errorf("parse database metadata from the snapshot copy: %w", err)
	}
	return metas, nil
}
