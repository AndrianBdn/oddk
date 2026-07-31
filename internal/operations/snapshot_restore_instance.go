package operations

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store"
	"github.com/andrianbdn/oddk/internal/store/instances"
	"github.com/andrianbdn/oddk/internal/util"
	"github.com/andrianbdn/oddk/internal/version"
)

// RestoreInstanceParams describes restoring ONE instance out of a snapshot
// archive into a LIVE deployment.
//
// This is the counterpart to `snapshot apply`, not a variant of it. Apply
// rebuilds a whole host, runs daemon-less, and demands an empty deployment
// because it replaces oddk.db and master.key wholesale. This restores a single
// instance into a deployment that keeps running, so it runs THROUGH the daemon
// (for executor serialization and health-check pausing) and touches neither the
// live oddk.db's other rows nor the live master.key.
type RestoreInstanceParams struct {
	ArchivePath  string
	InstanceName string

	// MasterKeyPath is the SOURCE host's master.key. Empty means "use this
	// host's own key", which is correct for the common case of restoring an
	// instance from a snapshot this same host took.
	MasterKeyPath string

	BackupDir string

	// Progress receives human-readable lines. Nil is fine (emitLine no-ops).
	Progress io.Writer
}

// RestoreInstanceResult describes what was rebuilt.
type RestoreInstanceResult struct {
	Instance   string    `json:"instance"`
	Created    bool      `json:"created"`
	Replaced   bool      `json:"replaced"`
	Databases  int       `json:"databases"`
	SourceHost string    `json:"sourceHost"`
	SnapshotAt time.Time `json:"snapshotAt"`
	Port       int       `json:"port"`
	CPUCores   int       `json:"cpuCores"`
	RAMMB      int       `json:"ramMb"`
	Image      string    `json:"image"`

	// PasswordChanged reports that the instance's stored postgres password was
	// replaced by the snapshot's. This is not optional and not a side effect to
	// hide: globals.sql sets the postgres role to the SOURCE's hash, so the only
	// plaintext that can still authenticate is the source's.
	PasswordChanged bool `json:"passwordChanged"`
}

// RestoreInstanceFromSnapshot rebuilds one instance from a snapshot archive.
//
// If the instance does not exist it is created from the archive's instance.json.
// If it does exist, its cluster is destroyed and rebuilt — the data is replaced,
// not merged, because RestoreClusterFromArchive requires an empty cluster and a
// partial overlay would silently mix two deployments' data.
//
// The instance's recorded configuration (port, resources, image, parameter
// group) comes from the SNAPSHOT, not from the live row. A restore that
// resurrected the data but kept a since-changed shape would not be a restore of
// that instance. Callers must show the operator what will be applied.
func RestoreInstanceFromSnapshot(ctx context.Context, deps *Dependencies, params *RestoreInstanceParams) (result *RestoreInstanceResult, err error) {
	if params.InstanceName == "" {
		return nil, operr.Invalidf("instance name is required")
	}
	if err := util.ValidateInstanceName(params.InstanceName); err != nil {
		return nil, operr.Invalidf("invalid instance name: %v", err)
	}

	// 1. Manifest first — it is the archive's first member, so compatibility and
	//    "is this instance even in here" cost kilobytes rather than the whole
	//    archive. Same reasoning as PreflightSnapshotApply.
	manifest, err := ReadSnapshotManifestFromArchive(params.ArchivePath)
	if err != nil {
		return nil, operr.Invalidf("cannot read snapshot manifest: %v", err)
	}
	if manifest.FormatVersion > SnapshotFormatVersion {
		return nil, operr.Invalidf("snapshot uses archive format v%d but this ODDK understands only v%d; upgrade ODDK",
			manifest.FormatVersion, SnapshotFormatVersion)
	}
	if newer, ok := isVersionNewer(manifest.OddkVersion, version.Version); ok && newer {
		return nil, operr.Invalidf("snapshot was created by oddk %s but this binary is %s; upgrade ODDK to %s or later",
			manifest.OddkVersion, version.Version, manifest.OddkVersion)
	}

	entry, found := snapshotEntry(manifest, params.InstanceName)
	if !found {
		return nil, operr.NotFoundf("snapshot does not contain instance %q (it has: %s)",
			params.InstanceName, instanceNameList(manifest))
	}
	if !entry.HasData {
		// A configuration-only entry has instance.json but no databases. Building
		// an empty cluster from it would produce an instance that looks healthy
		// and has silently lost its data — the same trap apply refuses.
		return nil, operr.Invalidf(
			"instance %q was captured configuration-only (%s), so the snapshot holds no data for it; restoring would create an empty cluster that looks healthy",
			params.InstanceName, entrySkipReason(entry))
	}

	// 2. Does it exist here? Decides create-vs-replace, and the port check below.
	//
	// Instances.Get signals "no such instance" with an operr.ErrNotFound error,
	// NOT with (nil, nil) — so absence must be matched on the marker. Treating
	// any error as fatal here made the create path dead code: restoring an
	// instance that does not exist locally, which is the whole point of
	// restoring after a destroy, always failed.
	existing, err := deps.Store.Instances.Get(params.InstanceName)
	if err != nil && !errors.Is(err, operr.ErrNotFound) {
		return nil, fmt.Errorf("read existing instance: %w", err)
	}
	if errors.Is(err, operr.ErrNotFound) {
		existing = nil
	}

	// 3. The image must be present before anything destructive. A missing image
	//    discovered after the cluster is torn down would leave the instance with
	//    no data and no container.
	if _, err := ensureImagesPresent(ctx, deps.Docker, []SnapshotInstanceEntry{entry}, params.Progress, nil); err != nil {
		return nil, err
	}

	// 4. Extract into the backup dir, where the daemon's startup sweep reclaims
	//    orphans (staleBackupArtifactPrefixes covers ".snapshot-").
	extractedDir, err := os.MkdirTemp(params.BackupDir, SnapshotStagingPrefix+"restore-*")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(extractedDir) }()

	if err := compression.NewCompressor().ExtractTarZstd(ctx, params.ArchivePath, extractedDir); err != nil {
		return nil, fmt.Errorf("extract snapshot: %w", err)
	}
	emitLine(params.Progress, "  ✓ Snapshot extracted")

	instanceDir := filepath.Join(extractedDir, snapshotInstancesDir, params.InstanceName)
	meta, metaFound, err := readInstanceMetadata(instanceDir)
	if err != nil {
		return nil, err
	}
	if !metaFound {
		return nil, operr.Invalidf("snapshot lists instance %s but the archive has no %s for it",
			params.InstanceName, instanceMetadataFile)
	}
	// Everything downstream mixes two identities — params.InstanceName drives the
	// store writes and the archive directory, meta.Name drives the container and
	// the status bookkeeping. They are the same in any archive ODDK produced, but
	// nothing has proved it, and a mismatch would write one instance's row while
	// building another's cluster.
	if meta.Name != params.InstanceName {
		return nil, operr.Invalidf(
			"archive is inconsistent: instances/%s/%s declares the instance name %q",
			params.InstanceName, instanceMetadataFile, meta.Name)
	}

	// 5. Resolve the key that decrypts the SNAPSHOT's stored credential. The
	//    live host's key is the right default (restoring this host's own
	//    snapshot); a snapshot from elsewhere needs its own key supplied.
	masterKey := deps.MasterKey
	if params.MasterKeyPath != "" {
		masterKey, err = crypto.ReadKeyFileAt(params.MasterKeyPath)
		if err != nil {
			return nil, operr.Invalidf("read source master key: %v", err)
		}
	}

	snapshotDB := filepath.Join(extractedDir, snapshotStoreFile)
	password, err := snapshotInstancePassword(snapshotDB, params.InstanceName, masterKey)
	if err != nil {
		return nil, err
	}
	emitLine(params.Progress, "  ✓ Source credential recovered")

	// 6. Remaining read-only guards, cheapest last because each needs the
	//    extracted tree. All of them refuse before anything is destroyed.
	metas := []*InstanceMeta{meta}
	if err := checkHostResources(metas); err != nil {
		return nil, err
	}
	if err := checkLocaleProviders(extractedDir, metas); err != nil {
		return nil, err
	}
	if err := checkRestorePortFree(deps, meta, existing); err != nil {
		return nil, err
	}

	dbs, dbsFound, err := readDatabaseMetadata(instanceDir)
	if err != nil {
		return nil, err
	}
	if !dbsFound {
		return nil, operr.Invalidf("archive has no %s for instance %s", databaseMetadataFile, params.InstanceName)
	}
	roleNames, err := roleNamesFromGlobals(filepath.Join(instanceDir, "globals.sql"))
	if err != nil {
		return nil, err
	}

	// 7. Resolve the parameter group NOW, not at container-create time. It can
	//    refuse — a snapshot that recorded only the group's name needs a group
	//    of that name to exist here — and every other guard is deliberately
	//    above the destructive marker. Resolving it late would mean discovering
	//    the refusal after the data volume had already been removed.
	parameterGroup := meta.ParameterGroupDefinition
	if parameterGroup == nil {
		group, groupErr := deps.Store.Parameters.GetGroup(meta.ParameterGroup)
		if groupErr != nil {
			return nil, operr.Invalidf("parameter group %q is neither inlined in the snapshot nor present on this host: %v",
				meta.ParameterGroup, groupErr)
		}
		parameterGroup = group
	} else if exists, existsErr := deps.Store.Parameters.GroupExists(meta.ParameterGroup); existsErr == nil && !exists {
		// The container is built from the INLINED definition, but the instance
		// row stores only the group's NAME. If this host has no group by that
		// name the row points at nothing: 'oddk parameters get' cannot show it
		// and a later 'instance apply'/reconfigure fails. Materialise the
		// snapshot's definition under that name so the row and reality agree.
		if err := deps.Store.Parameters.CreateGroup(meta.ParameterGroup, parameterGroup.Parameters); err != nil {
			return nil, fmt.Errorf("materialise parameter group %q from the snapshot: %w", meta.ParameterGroup, err)
		}
		emitLine(params.Progress, "  ✓ Parameter group %q restored from the snapshot", meta.ParameterGroup)
	}

	// 8. Re-encrypt under THIS host's key before writing anything. The plaintext
	//    is what must reach POSTGRES_PASSWORD (it is the one matching
	//    globals.sql's hash); the master key only decides what it is encrypted
	//    under at rest. Doing this before the destructive phase means a key
	//    problem cannot strand a torn-down instance.
	encrypted, err := crypto.EncryptPassword(password, deps.MasterKey)
	if err != nil {
		return nil, fmt.Errorf("re-encrypt password under this host's key: %w", err)
	}

	result = &RestoreInstanceResult{
		Instance:        meta.Name,
		Created:         existing == nil,
		Replaced:        existing != nil,
		SourceHost:      manifest.SourceHost,
		SnapshotAt:      manifest.CreatedAt,
		Port:            meta.Port,
		CPUCores:        meta.CPUCores,
		RAMMB:           meta.RAMMB,
		Image:           meta.Image,
		PasswordChanged: true,
	}

	// ---- Destructive from here ----

	// Mark restoring before any Docker work, and error on every failure path, so
	// an interrupted restore cannot leave a half-built cluster that the health
	// check (a bare connect+ping) reports as green. reconcileInstances converts
	// a stuck "restoring" to "error".
	if existing != nil {
		if err := deps.Store.Instances.UpdateStatus(meta.Name, "restoring"); err != nil {
			return nil, fmt.Errorf("mark instance restoring: %w", err)
		}
	} else {
		if _, err := deps.Store.Instances.Create(
			meta.Name, meta.Port, meta.Version, encrypted, "",
			meta.CPUCores, meta.RAMMB, meta.ParameterGroup, meta.Image,
		); err != nil {
			return nil, fmt.Errorf("create instance row: %w", err)
		}
		if err := deps.Store.Instances.UpdateStatus(meta.Name, "restoring"); err != nil {
			return nil, fmt.Errorf("mark instance restoring: %w", err)
		}
	}

	defer func() {
		if err == nil {
			return
		}
		if statusErr := deps.Store.Instances.UpdateStatus(meta.Name, "error"); statusErr != nil {
			emitLine(params.Progress, "  (also failed to mark %s as error: %v)", meta.Name, statusErr)
		}
	}()

	if existing != nil {
		// Tear the old cluster down. RestoreClusterFromArchive requires an empty
		// cluster; overlaying onto the existing one would mix two deployments'
		// data with no way to tell them apart afterwards.
		if err := destroyInstanceCluster(deps, existing, params.Progress); err != nil {
			return nil, err
		}
		// The stored password must become the snapshot's: globals.sql rewrites
		// the postgres role to the source's hash, so the live host's old
		// plaintext would stop authenticating the moment the restore lands.
		if err := deps.Store.Instances.UpdatePassword(meta.Name, encrypted); err != nil {
			return nil, fmt.Errorf("store recovered password: %w", err)
		}
		if err := applySnapshotShape(deps, meta); err != nil {
			return nil, err
		}
	}

	containerID, err := deps.Docker.CreateContainer(
		meta.Name, meta.Version, meta.Image, meta.Port, password,
		meta.CPUCores, meta.RAMMB, meta.ParameterGroup, parameterGroup.Parameters,
	)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}
	if err := deps.Store.Instances.UpdateContainerID(meta.Name, containerID); err != nil {
		return nil, fmt.Errorf("record container id: %w", err)
	}
	if err := deps.Docker.StartContainer(containerID); err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}
	emitLine(params.Progress, "  ✓ Volume and container created")

	if err := waitForPostgresReady(ctx, meta.Port, password); err != nil {
		return nil, fmt.Errorf("cluster did not become ready: %w", err)
	}
	emitLine(params.Progress, "  ✓ PostgreSQL ready")

	restored, err := RestoreClusterFromArchive(ctx, deps, RestoreClusterParams{
		InstanceName:  meta.Name,
		Image:         meta.Image,
		Port:          meta.Port,
		Password:      password,
		CPUCores:      meta.CPUCores,
		ExtractedDir:  instanceDir,
		Databases:     dbs,
		ExpectedRoles: roleNames,
	})
	if err != nil {
		return nil, err // the deferred handler records "error"
	}
	result.Databases = restored
	emitLine(params.Progress, "  ✓ Roles and %d database(s) restored", restored)

	if err := deps.Store.Instances.UpdateStatus(meta.Name, "running"); err != nil {
		return nil, fmt.Errorf("mark instance running: %w", err)
	}
	return result, nil
}

// destroyInstanceCluster removes an existing instance's container and data
// volume so the restore starts from an empty cluster.
//
// A missing container is not an error: the instance may already be in "error"
// with nothing built, which is exactly when someone reaches for a restore.
func destroyInstanceCluster(deps *Dependencies, existing *instances.RDBMSInstance, progress io.Writer) error {
	if existing.ContainerID != "" {
		if err := deps.Docker.StopContainer(existing.ContainerID); err != nil {
			// Already stopped is fine; a genuine failure surfaces on remove.
			emitLine(progress, "  … stop container: %v (continuing)", err)
		}
		if err := deps.Docker.RemoveContainer(existing.ContainerID); err != nil {
			return fmt.Errorf("remove existing container: %w", err)
		}
		if err := deps.Store.Instances.UpdateContainerID(existing.Name, ""); err != nil {
			return fmt.Errorf("clear container id: %w", err)
		}
	}
	volumeName := fmt.Sprintf("oddk-data-%s", existing.Name)
	if err := deps.Docker.RemoveVolume(volumeName); err != nil {
		return fmt.Errorf("remove existing data volume %s: %w", volumeName, err)
	}
	emitLine(progress, "  ✓ Existing cluster removed")
	return nil
}

// applySnapshotShape brings a pre-existing row's recorded configuration in line
// with the snapshot's, so the restored instance is described the way it will
// actually run rather than the way it used to.
//
// All four properties matter, not just the visible ones: the container is
// created from meta, so a row left holding the old values would report a port
// nothing listens on, and IsPortInUse would then guard the wrong port for the
// next instance created.
func applySnapshotShape(deps *Dependencies, meta *InstanceMeta) error {
	if err := deps.Store.Instances.UpdateImage(meta.Name, meta.Image, meta.Version); err != nil {
		return fmt.Errorf("record snapshot image: %w", err)
	}
	if err := deps.Store.Instances.UpdateParameterGroup(meta.Name, meta.ParameterGroup); err != nil {
		return fmt.Errorf("record snapshot parameter group: %w", err)
	}
	if err := deps.Store.Instances.UpdateResources(meta.Name, meta.Port, meta.CPUCores, meta.RAMMB); err != nil {
		return fmt.Errorf("record snapshot resources: %w", err)
	}
	return nil
}

// checkRestorePortFree refuses a port another instance holds, and — for a brand
// new instance — a port anything on this host already answers on.
//
// An existing instance legitimately holds its own port: its container is torn
// down before the rebuild, so a live listener there is expected, not a conflict.
func checkRestorePortFree(deps *Dependencies, meta *InstanceMeta, existing *instances.RDBMSInstance) error {
	inUse, holder, err := deps.Store.Instances.IsPortInUse(meta.Port)
	if err != nil {
		return fmt.Errorf("check port availability: %w", err)
	}
	if inUse && holder != meta.Name {
		return operr.Conflictf("instance %q was on port %d in the snapshot, but instance %q holds that port on this host",
			meta.Name, meta.Port, holder)
	}
	// Probe the host for a live listener whenever the port is not one this
	// instance already holds. An existing instance legitimately occupies its OWN
	// port (its container is torn down before the rebuild), but if the snapshot
	// moves it to a DIFFERENT port, that port is as unproven as a new instance's
	// — and discovering something else on it after the volume is destroyed is
	// exactly the failure this ordering exists to prevent.
	if existing == nil || existing.Port != meta.Port {
		return checkPortsAvailable([]*InstanceMeta{meta})
	}
	return nil
}

// snapshotInstancePassword recovers one instance's plaintext postgres password
// from the snapshot's embedded oddk.db.
//
// This is what makes the restore work at all: globals.sql carries only the hash,
// so the cluster must be initialised with the very plaintext the source used, or
// the restored postgres role authenticates nothing.
func snapshotInstancePassword(snapshotDBPath, instanceName string, masterKey []byte) (string, error) {
	st, err := store.NewStore(snapshotDBPath, filepath.Dir(snapshotDBPath))
	if err != nil {
		return "", fmt.Errorf("open snapshot store: %w", err)
	}
	defer func() { _ = st.Sqlx.Close() }()

	row, err := st.Instances.Get(instanceName)
	if err != nil {
		return "", fmt.Errorf("read snapshot instance row: %w", err)
	}
	if row == nil {
		return "", operr.Invalidf("instance %q is in the snapshot archive but not in its oddk.db", instanceName)
	}
	if row.Password == "" {
		return "", operr.Invalidf("instance %q has no stored credential in the snapshot", instanceName)
	}

	password, err := crypto.DecryptPassword(row.Password, masterKey)
	if err != nil {
		return "", operr.Invalidf(
			"master key mismatch: this key cannot decrypt the snapshot's credential for %q. Restoring with the wrong key would create a cluster whose postgres password ODDK cannot recover. Pass the source host's key with --master-key",
			instanceName)
	}
	return password, nil
}

// ensureImagesPresent pulls any image the given entries need that this host does
// not already have.
//
// Shared with snapshot apply: a missing image discovered after the destructive
// phase aborts a restore that may not be retryable.
func ensureImagesPresent(
	ctx context.Context,
	dockerClient *docker.Client,
	entries []SnapshotInstanceEntry,
	progress, pullProgress io.Writer,
) (pulled []string, err error) {
	if dockerClient == nil {
		return nil, fmt.Errorf("no Docker client available to verify instance images")
	}
	seen := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.HasData || entry.Image == "" {
			continue
		}
		if _, dup := seen[entry.Image]; dup {
			continue
		}
		seen[entry.Image] = struct{}{}

		if _, exists := dockerClient.CheckImageExists(entry.Image); exists {
			continue
		}
		emitLine(progress, "  … pulling %s (not present on this host)", entry.Image)
		if err := dockerClient.PullImageProgress(ctx, entry.Image, pullProgress); err != nil {
			return pulled, fmt.Errorf("instance %q needs image %s, which is not present locally and could not be pulled: %w",
				entry.Name, entry.Image, err)
		}
		pulled = append(pulled, entry.Image)
	}
	return pulled, nil
}

func snapshotEntry(manifest *SnapshotManifest, name string) (SnapshotInstanceEntry, bool) {
	for _, entry := range manifest.Instances {
		if entry.Name == name {
			return entry, true
		}
	}
	return SnapshotInstanceEntry{}, false
}

func instanceNameList(manifest *SnapshotManifest) string {
	if len(manifest.Instances) == 0 {
		return "none"
	}
	names := make([]string, 0, len(manifest.Instances))
	for _, entry := range manifest.Instances {
		names = append(names, entry.Name)
	}
	return strings.Join(names, ", ")
}

func entrySkipReason(entry SnapshotInstanceEntry) string {
	if entry.SkipReason == "" {
		return "no reason recorded"
	}
	return entry.SkipReason
}
