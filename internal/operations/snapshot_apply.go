package operations

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store"
	"github.com/andrianbdn/oddk/internal/util"
	"github.com/andrianbdn/oddk/internal/version"
)

// gatewayIP is the oddk-bridge gateway that every instance binds to. It is
// host-local, so an answer on it means that port is genuinely taken here.
const gatewayIP = "10.88.0.1"

// SnapshotApplyParams configures a whole-deployment restore.
type SnapshotApplyParams struct {
	ArchivePath   string
	MasterKeyPath string
	DataDir       string
	BackupDir     string
	DaemonPort    int
	Docker        *docker.Client

	// Progress receives human-readable status lines during preflight (nil
	// discards them).
	Progress io.Writer

	// PullProgress receives raw Docker pull frames, for a caller that wants to
	// render layer progress. nil discards them; an image pull on a DR host can
	// take minutes, so a CLI should supply one.
	PullProgress io.Writer
}

// PreflightCheck is one line of the pre-apply report. Every check runs before
// anything on the host is modified.
type PreflightCheck struct {
	Label  string
	OK     bool
	Detail string
}

// SnapshotApplyPlan is the validated result of preflight — everything the
// destructive phase needs, resolved while the host is still untouched.
type SnapshotApplyPlan struct {
	Manifest     *SnapshotManifest
	ExtractedDir string
	MasterKey    []byte
	Instances    []*InstanceMeta
	Checks       []PreflightCheck

	// ExistingKeyPath is set when the target already has a master.key that
	// will be displaced (the normal case: the installer started the daemon,
	// which generated one).
	ExistingKeyPath string

	// PulledImages lists images preflight fetched onto this host. Recorded so
	// the summary can be honest that preflight was not purely read-only.
	PulledImages []string

	params *SnapshotApplyParams
}

// Cleanup removes the extracted staging tree. Safe to call more than once.
func (p *SnapshotApplyPlan) Cleanup() {
	if p != nil && p.ExtractedDir != "" {
		_ = os.RemoveAll(p.ExtractedDir)
	}
}

// SnapshotApplyResult reports what was rebuilt.
type SnapshotApplyResult struct {
	Restored       []string // instances rebuilt with their data
	ConfigOnly     []string // instances whose snapshot held no data
	ReplacedKeyAs  string
	ReplacedDBAs   string
	TokensReplaced bool

	// Backup catalogue reconciliation. BackupsDangling counts records left with
	// neither a local nor a remote copy — the daemon's startup sweep will drop
	// those, so the operator is told rather than finding history gone.
	BackupsRepointed    int
	BackupsLocalCleared int
	BackupsDangling     int

	// Snapshot catalogue reconciliation, same shape and same reason: the
	// restored rows carry the source host's paths.
	SnapshotsRepointed    int
	SnapshotsLocalCleared int
	SnapshotsDangling     int
}

// PreflightSnapshotApply validates that this snapshot can be applied to this
// host and prepares everything the destructive phase needs. It modifies
// nothing. On success the caller owns plan.ExtractedDir and must Cleanup().
//
// Checks run cheapest-first so an obvious refusal (wrong version, running
// daemon, non-empty deployment) costs a second rather than the time to
// decompress the whole archive.
func PreflightSnapshotApply(ctx context.Context, params *SnapshotApplyParams) (*SnapshotApplyPlan, error) {
	plan := &SnapshotApplyPlan{params: params}
	fail := func(label, detail string, err error) (*SnapshotApplyPlan, error) {
		plan.Checks = append(plan.Checks, PreflightCheck{Label: label, OK: false, Detail: detail})
		plan.Cleanup()
		return plan, appendNoChanges(err)
	}
	pass := func(label, detail string) {
		plan.Checks = append(plan.Checks, PreflightCheck{Label: label, OK: true, Detail: detail})
	}

	// 1. Version compatibility, read from the archive's first member so this
	//    costs kilobytes rather than the whole archive.
	manifest, err := ReadSnapshotManifestFromArchive(params.ArchivePath)
	if err != nil {
		return fail("Snapshot is readable", "", fmt.Errorf("cannot read snapshot manifest: %w", err))
	}
	plan.Manifest = manifest

	if manifest.FormatVersion > SnapshotFormatVersion {
		return fail("Snapshot format is supported",
			fmt.Sprintf("archive format v%d, this binary understands v%d", manifest.FormatVersion, SnapshotFormatVersion),
			operr.Invalidf("snapshot uses archive format v%d but this ODDK understands only v%d; upgrade ODDK",
				manifest.FormatVersion, SnapshotFormatVersion))
	}
	if newer, cmpOK := isVersionNewer(manifest.OddkVersion, version.Version); cmpOK && newer {
		return fail("Snapshot version is supported",
			fmt.Sprintf("created by oddk %s, this binary is %s", manifest.OddkVersion, version.Version),
			operr.Invalidf("snapshot was created by oddk %s but this binary is %s; upgrade ODDK to %s or later",
				manifest.OddkVersion, version.Version, manifest.OddkVersion))
	}
	pass("Snapshot version is supported",
		fmt.Sprintf("snapshot %s (this binary: %s)", manifest.OddkVersion, version.Version))

	// 2. The daemon must not be running: it and this command would both write
	//    oddk.db and drive Docker.
	if daemonIsListening(params.DaemonPort) {
		return fail("Daemon is not running", fmt.Sprintf("something is listening on 127.0.0.1:%d", params.DaemonPort),
			operr.Conflictf("the ODDK daemon appears to be running on 127.0.0.1:%d; stop it first (systemctl stop oddk)", params.DaemonPort))
	}
	pass("Daemon is not running", "")

	// 3. The target deployment must be empty. Applying installs the snapshot's
	//    master.key, which would leave any pre-existing instance's stored
	//    password undecryptable.
	existing, err := existingInstanceNames(params.DataDir)
	if err != nil {
		return fail("Target deployment is empty", "", err)
	}
	if len(existing) > 0 {
		return fail("Target deployment is empty", fmt.Sprintf("%d instance(s): %s", len(existing), strings.Join(existing, ", ")),
			operr.Conflictf("snapshot apply replaces oddk.db and master.key wholesale, which would leave the existing instances (%s) with undecryptable stored passwords. Destroy them first ('oddk instance destroy <name> --force' with the daemon running), or apply to a fresh host. If a previous apply failed part-way and these instances have no containers, remove %s and retry",
				strings.Join(existing, ", "), filepath.Join(params.DataDir, "oddk.db")))
	}
	keyPath := filepath.Join(params.DataDir, "master.key")
	if _, statErr := os.Stat(keyPath); statErr == nil {
		plan.ExistingKeyPath = keyPath
		pass("Target deployment is empty", "existing master.key will be replaced")
	} else {
		pass("Target deployment is empty", "")
	}

	// 4. Every image the rebuild needs must be present, pulled now if not.
	//    This runs BEFORE extraction because the manifest already names the
	//    images: a fresh DR host has an empty image cache (the installer never
	//    pulls), and discovering that inside the destructive phase would abort
	//    an apply that has already replaced oddk.db and master.key.
	pulled, err := ensureSnapshotImages(ctx, params, manifest)
	if err != nil {
		return fail("Instance images are available", "", err)
	}
	plan.PulledImages = pulled
	pass("Instance images are available", imageSummary(manifest, pulled))

	// 5. Extract. Everything above was cheap; this is where real work starts,
	//    but it still only writes into a staging directory.
	//
	//    Staged under BackupDir, not DataDir, so the daemon's startup sweep
	//    reclaims it if this process is killed — staleBackupArtifactPrefixes
	//    already covers the ".snapshot-" prefix, and it only scans BackupDir.
	extractedDir, err := os.MkdirTemp(params.BackupDir, SnapshotStagingPrefix+"apply-*")
	if err != nil {
		return fail("Snapshot extracted", "", fmt.Errorf("create staging dir: %w", err))
	}
	plan.ExtractedDir = extractedDir
	if err := compression.NewCompressor().ExtractTarZstd(ctx, params.ArchivePath, extractedDir); err != nil {
		return fail("Snapshot extracted", "", fmt.Errorf("extract snapshot: %w", err))
	}
	snapshotDB := filepath.Join(extractedDir, snapshotStoreFile)
	if _, err := os.Stat(snapshotDB); err != nil {
		return fail("Snapshot extracted", "", fmt.Errorf("snapshot archive has no %s: %w", snapshotStoreFile, err))
	}
	pass("Snapshot extracted", "")

	// 6. The master key must decrypt the snapshot's stored credentials.
	//    Without it, apply would build clusters whose postgres password cannot
	//    be recovered (globals.sql carries only the hash).
	masterKey, err := crypto.ReadKeyFileAt(params.MasterKeyPath)
	if err != nil {
		return fail("Master key decrypts instance credentials", "", err)
	}
	plan.MasterKey = masterKey
	if err := verifyMasterKeyAgainstSnapshot(snapshotDB, masterKey); err != nil {
		return fail("Master key decrypts instance credentials", "", err)
	}
	pass("Master key decrypts instance credentials", "")

	// 7. Load each instance's configuration from the archive.
	instances, err := loadSnapshotInstances(extractedDir, manifest)
	if err != nil {
		return fail("Instance configuration is readable", "", err)
	}
	plan.Instances = instances

	// 8. This host must be able to run the recorded sizes. The values are not
	//    just cgroup limits: RAMMB drives shared_buffers and shm_size, so a
	//    smaller DR host would build a container PostgreSQL cannot start in —
	//    and would discover it only after the destructive phase.
	if err := checkHostResources(instances); err != nil {
		return fail("Host can run the recorded instance sizes", "", err)
	}
	pass("Host can run the recorded instance sizes", resourceSummary(instances))

	// 9. Every database must use a locale provider this restore can reproduce.
	//    buildCreateDatabaseSQL only reproduces libc locales; an ICU/builtin
	//    database would come back with silently different collation.
	if err := checkLocaleProviders(extractedDir, instances); err != nil {
		return fail("Database locales can be reproduced", "", err)
	}
	pass("Database locales can be reproduced", "")

	// 10. Ports must be free.
	if err := checkPortsAvailable(instances); err != nil {
		return fail("Instance ports are available", "", err)
	}
	pass("Instance ports are available", portList(instances))

	return plan, nil
}

// noChangesSuffix closes every preflight refusal. Preflight may pull Docker
// images (additive and reversible), so the promise is specifically about
// ODDK's own state rather than the host in general.
const noChangesSuffix = "No ODDK state on this host was modified."

// appendNoChanges guarantees the promise appears on EVERY refusal path rather
// than only the ones someone remembered to write it into.
func appendNoChanges(err error) error {
	if err == nil || strings.Contains(err.Error(), noChangesSuffix) {
		return err
	}
	return fmt.Errorf("%w. %s", err, noChangesSuffix)
}

// ReadSnapshotManifestFromArchive streams the archive only as far as its
// manifest, which MakeSnapshot writes first precisely so this is cheap.
func ReadSnapshotManifestFromArchive(archivePath string) (*SnapshotManifest, error) {
	f, err := os.Open(archivePath) // #nosec G304 - operator-supplied snapshot path
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	zr, err := zstd.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("open zstd stream: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s not found in archive (is this an ODDK snapshot?)", snapshotManifestFile)
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Name != snapshotManifestFile {
			continue
		}
		// Bounded: a manifest is kilobytes; anything larger is not one.
		data, err := io.ReadAll(io.LimitReader(tr, 8<<20))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", snapshotManifestFile, err)
		}
		var manifest SnapshotManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("parse %s: %w", snapshotManifestFile, err)
		}
		return &manifest, nil
	}
}

// isVersionNewer reports whether semantic version a is newer than b. The bool
// is false when either side cannot be parsed, in which case callers skip the
// check rather than refuse on a version string they do not understand.
func isVersionNewer(a, b string) (newer, ok bool) {
	pa, okA := parseSemver(a)
	pb, okB := parseSemver(b)
	if !okA || !okB {
		return false, false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i], true
		}
	}
	return false, true
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		// Tolerate a pre-release/build suffix on the patch component.
		if idx := strings.IndexAny(p, "-+"); idx >= 0 {
			p = p[:idx]
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// daemonIsListening reports whether anything answers on the daemon's port.
func daemonIsListening(port int) bool {
	if port == 0 {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// existingInstanceNames returns the instances already configured on this host.
//
// It deliberately does NOT use store.NewStore, which is a writing constructor:
// it runs migrations, seeds the default parameter group and KV defaults, and
// takes the app_migrations_lock. Preflight promises it changes nothing, and a
// process killed inside that lock window would leave the lock row behind and
// prevent the daemon from ever starting. A bare read-only query keeps the
// promise true.
func existingInstanceNames(dataDir string) ([]string, error) {
	dbPath := filepath.Join(dataDir, "oddk.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil // fresh host, nothing configured
	}

	db, err := sqlx.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open existing store read-only: %w", err)
	}
	defer func() { _ = db.Close() }()

	var names []string
	if err := db.Select(&names, `SELECT name FROM rdbms_instances ORDER BY name`); err != nil {
		// A database with no rdbms_instances table is not a usable ODDK store;
		// treat it as an empty deployment rather than blocking recovery on it.
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, fmt.Errorf("list existing instances: %w", err)
	}
	return names, nil
}

// verifyMasterKeyAgainstSnapshot proves the supplied key belongs to this
// snapshot by decrypting a stored credential with it.
//
// This is the check that makes the key genuinely required: applying without
// the right key would create clusters with a fresh random postgres password,
// then globals.sql would overwrite the postgres role with the source's hash —
// leaving an instance ODDK can never connect to again.
func verifyMasterKeyAgainstSnapshot(snapshotDBPath string, masterKey []byte) error {
	st, err := store.NewStore(snapshotDBPath, filepath.Dir(snapshotDBPath))
	if err != nil {
		return fmt.Errorf("open snapshot store: %w", err)
	}
	defer func() { _ = st.Sqlx.Close() }()

	list, err := st.Instances.List()
	if err != nil {
		return fmt.Errorf("read snapshot instances: %w", err)
	}
	for i := range list {
		if list[i].Password == "" {
			continue
		}
		if _, err := crypto.DecryptPassword(list[i].Password, masterKey); err != nil {
			return operr.Invalidf("master key mismatch: this key cannot decrypt the snapshot's stored credentials. Applying it would create clusters whose postgres password ODDK cannot recover. Nothing was modified")
		}
		return nil
	}
	// No instance carries a credential (an empty deployment) — nothing to
	// verify against, and nothing that could be bricked either.
	return nil
}

// loadSnapshotInstances reads instance.json for every instance the manifest
// lists, so the destructive phase never has to discover a missing file.
func loadSnapshotInstances(extractedDir string, manifest *SnapshotManifest) ([]*InstanceMeta, error) {
	out := make([]*InstanceMeta, 0, len(manifest.Instances))
	for _, entry := range manifest.Instances {
		dir := filepath.Join(extractedDir, snapshotInstancesDir, entry.Name)
		meta, found, err := readInstanceMetadata(dir)
		if err != nil {
			return nil, fmt.Errorf("instance %s: %w", entry.Name, err)
		}
		if !found {
			return nil, fmt.Errorf("snapshot lists instance %s but the archive has no %s for it",
				entry.Name, instanceMetadataFile)
		}
		out = append(out, meta)
	}
	return out, nil
}

// checkPortsAvailable refuses if two instances collide, or if something on this
// host already answers on an instance's port.
func checkPortsAvailable(instances []*InstanceMeta) error {
	seen := make(map[int]string, len(instances))
	for _, meta := range instances {
		if other, dup := seen[meta.Port]; dup {
			return operr.Invalidf("snapshot is inconsistent: instances %s and %s both use port %d. Nothing was modified",
				other, meta.Name, meta.Port)
		}
		seen[meta.Port] = meta.Name

		// Instances bind the bridge gateway, which is host-local. If the bridge
		// does not exist yet the dial simply fails, which is the correct answer:
		// nothing can be listening there.
		addr := net.JoinHostPort(gatewayIP, strconv.Itoa(meta.Port))
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return operr.Conflictf("instance %q needs port %d but something already answers on %s; free it, or destroy whatever holds it. Nothing was modified",
				meta.Name, meta.Port, addr)
		}
	}
	return nil
}

func portList(instances []*InstanceMeta) string {
	parts := make([]string, 0, len(instances))
	for _, meta := range instances {
		parts = append(parts, strconv.Itoa(meta.Port))
	}
	return strings.Join(parts, ", ")
}

// ExecuteSnapshotApply performs the destructive phase: it installs the
// snapshot's oddk.db and master.key over this host's, then rebuilds every
// instance that carries data.
//
// Everything before this point was read-only. Call it only with a plan from a
// successful PreflightSnapshotApply, and only after the operator has confirmed.
//
// The daemon check is repeated here, not merely inherited from preflight: the
// systemd unit sets Restart=always, so a daemon killed rather than stopped can
// return within seconds — potentially between preflight and now.
func ExecuteSnapshotApply(ctx context.Context, plan *SnapshotApplyPlan, progress io.Writer) (*SnapshotApplyResult, error) {
	params := plan.params
	if daemonIsListening(params.DaemonPort) {
		return nil, operr.Conflictf("the ODDK daemon started while preflight was running; stop it (systemctl stop oddk) and retry. %s", noChangesSuffix)
	}

	// Hold the data-dir lock for the whole destructive phase. The port probe
	// above is a point-in-time check and cannot stop a daemon that starts a
	// moment later (Restart=always brings a killed one back in 5s); the daemon
	// takes this same lock at startup, so while it is held it cannot come up
	// and race us — driving Docker, sweeping our helper containers, and writing
	// the oddk.db we are replacing.
	lock, err := store.AcquireDataDirLock(params.DataDir)
	if err != nil {
		return nil, operr.Conflictf("%v. %s", err, noChangesSuffix)
	}
	defer lock.Release()

	result := &SnapshotApplyResult{}
	stamp := time.Now().UTC().Format("20060102150405")

	// 1. Install master.key, displacing any generated one. The old key is
	//    renamed rather than deleted: the empty-deployment guard means it
	//    protects nothing, but destroying key material silently is not a habit
	//    worth having.
	keyPath := filepath.Join(params.DataDir, "master.key")
	if plan.ExistingKeyPath != "" {
		replaced := keyPath + ".replaced-" + stamp
		if err := os.Rename(keyPath, replaced); err != nil {
			return nil, fmt.Errorf("set aside existing master.key: %w", err)
		}
		result.ReplacedKeyAs = replaced
	}
	if err := crypto.WriteKeyFile(keyPath, plan.MasterKey); err != nil {
		return nil, err
	}
	emitLine(progress, "  ✓ master.key installed%s", suffixIf(result.ReplacedKeyAs, " (previous key saved as %s)", filepath.Base(result.ReplacedKeyAs)))

	// 2. Install oddk.db. Any pre-existing one belongs to an empty deployment
	//    (preflight proved it), but it holds the token the operator just minted
	//    to verify the install, so keep it rather than drop it.
	dbPath := filepath.Join(params.DataDir, "oddk.db")
	if _, err := os.Stat(dbPath); err == nil {
		replaced := dbPath + ".replaced-" + stamp
		if err := os.Rename(dbPath, replaced); err != nil {
			return nil, fmt.Errorf("set aside existing oddk.db: %w", err)
		}
		result.ReplacedDBAs = replaced
		result.TokensReplaced = true
		// WAL/SHM sidecars of the displaced database would otherwise be
		// misread as belonging to the newly installed one.
		for _, sidecar := range []string{"-wal", "-shm"} {
			_ = os.Remove(dbPath + sidecar)
		}
	}
	if err := copyFile(filepath.Join(plan.ExtractedDir, snapshotStoreFile), dbPath, 0o600); err != nil {
		return nil, fmt.Errorf("install oddk.db: %w", err)
	}
	emitLine(progress, "  ✓ oddk.db installed (%d instances)", len(plan.Manifest.Instances))

	// 3. Reopen the freshly installed store. Migrations run here, which is what
	//    lets an older snapshot land on a newer binary.
	st, err := store.NewStore(dbPath, params.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open restored store: %w", err)
	}
	defer func() { _ = st.Sqlx.Close() }()

	deps := &Dependencies{
		Store:     st,
		Docker:    params.Docker,
		MasterKey: plan.MasterKey,
		DataDir:   params.DataDir,
		BackupDir: params.BackupDir,
	}

	// 3b. Reconcile the restored backup catalogue against this host's backup
	//     directory. The rows carry the SOURCE host's paths and the archive
	//     carries no backup files, so without this the first ListAllBackups —
	//     run by daemon startup AND by 'oddk checklist', the two things this
	//     command tells the operator to do next — would hard-delete every
	//     local-only record and silently destroy the backup history.
	repointed, cleared, dangling, err := st.Backup.ReconcileLocalLocations(params.BackupDir)
	if err != nil {
		return nil, fmt.Errorf("reconcile backup history: %w", err)
	}
	result.BackupsRepointed = repointed
	result.BackupsLocalCleared = cleared
	result.BackupsDangling = dangling
	if repointed > 0 || cleared > 0 {
		emitLine(progress, "  ✓ Backup catalogue reconciled (%d re-pointed to this host, %d local copies not present here)",
			repointed, cleared)
	}

	// 3c. The SNAPSHOT catalogue needs the same treatment, for the same reason:
	//     its rows carry the source host's paths and this archive contains no
	//     other snapshots. Skipping it would leave 'oddk snapshot list' claiming
	//     local copies that were never on this machine, and retention operating
	//     on paths that do not exist here.
	snapRepointed, snapCleared, snapDangling, err := st.Snapshot.ReconcileLocalLocations(params.BackupDir)
	if err != nil {
		return nil, fmt.Errorf("reconcile snapshot history: %w", err)
	}
	result.SnapshotsRepointed = snapRepointed
	result.SnapshotsLocalCleared = snapCleared
	result.SnapshotsDangling = snapDangling
	if snapRepointed > 0 || snapCleared > 0 {
		emitLine(progress, "  ✓ Snapshot catalogue reconciled (%d re-pointed to this host, %d local copies not present here)",
			snapRepointed, snapCleared)
	}

	// 4. Rebuild each instance.
	byName := make(map[string]SnapshotInstanceEntry, len(plan.Manifest.Instances))
	for _, entry := range plan.Manifest.Instances {
		byName[entry.Name] = entry
	}

	for _, meta := range plan.Instances {
		entry := byName[meta.Name]
		if !entry.HasData {
			// No cluster is built for a configuration-only entry. Creating an
			// empty one would leave an instance that looks healthy but has lost
			// its data, which is far worse than one that visibly needs
			// attention.
			if err := markInstanceUnbuilt(st, meta.Name); err != nil {
				return nil, err
			}
			result.ConfigOnly = append(result.ConfigOnly, meta.Name)
			emitLine(progress, "  ○ %s: configuration only, no data in snapshot - left in 'error' for you to destroy or rebuild", meta.Name)
			continue
		}

		emitLine(progress, "Restoring instance %s (this may take a while)...", meta.Name)
		if err := rebuildInstanceFromSnapshot(ctx, deps, plan, meta, progress); err != nil {
			return nil, fmt.Errorf("restore instance %s: %w", meta.Name, err)
		}
		result.Restored = append(result.Restored, meta.Name)
	}

	return result, nil
}

// rebuildInstanceFromSnapshot creates the instance's volume and container with
// the password recorded in the restored store, waits for readiness, and replays
// its dump.
//
// The password is the crux: the cluster is initialised with the very plaintext
// the source host used, so the ALTER ROLE postgres inside globals.sql is a
// no-op and ODDK's stored credential stays valid. That is the whole reason the
// master key is required.
func rebuildInstanceFromSnapshot(
	ctx context.Context,
	deps *Dependencies,
	plan *SnapshotApplyPlan,
	meta *InstanceMeta,
	progress io.Writer,
) (err error) {
	instance, getErr := deps.Store.Instances.Get(meta.Name)
	if getErr != nil {
		return fmt.Errorf("read restored instance row: %w", getErr)
	}
	if instance == nil {
		return fmt.Errorf("instance %s is in the archive but not in the snapshot's oddk.db", meta.Name)
	}

	// The installed oddk.db carries the SOURCE host's status, which is
	// "running" for every instance being rebuilt here (only running instances
	// are captured with data). Mark it "restoring" before touching Docker so an
	// interrupted apply cannot leave a half-restored cluster recorded as
	// healthy — reconcileInstances converts a stuck "restoring" to "error", and
	// the health checker's bare connect-and-ping would otherwise report an
	// instance missing most of its databases as green.
	if err := deps.Store.Instances.UpdateStatus(meta.Name, "restoring"); err != nil {
		return fmt.Errorf("mark instance restoring: %w", err)
	}

	// Any failure below leaves a cluster that is absent, empty or partial.
	// Recording "error" on every one of them — not just the restore branch —
	// is what stops the daemon from later reporting it as running.
	defer func() {
		if err == nil {
			return
		}
		if statusErr := deps.Store.Instances.UpdateStatus(meta.Name, "error"); statusErr != nil {
			emitLine(progress, "  (also failed to mark %s as error: %v)", meta.Name, statusErr)
		}
	}()

	password, err := crypto.DecryptPassword(instance.Password, deps.MasterKey)
	if err != nil {
		return fmt.Errorf("decrypt password: %w", err)
	}

	parameterGroup := meta.ParameterGroupDefinition
	if parameterGroup == nil {
		// The archive recorded only the name (the group was unreadable at
		// backup time); fall back to whatever the restored store holds.
		group, groupErr := deps.Store.Parameters.GetGroup(meta.ParameterGroup)
		if groupErr != nil {
			return fmt.Errorf("parameter group %q is not in the snapshot or the restored store: %w", meta.ParameterGroup, groupErr)
		}
		parameterGroup = group
	}

	containerID, err := deps.Docker.CreateContainer(
		meta.Name, meta.Version, meta.Image, meta.Port, password,
		meta.CPUCores, meta.RAMMB, meta.ParameterGroup, parameterGroup.Parameters,
	)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := deps.Store.Instances.UpdateContainerID(meta.Name, containerID); err != nil {
		return fmt.Errorf("record container id: %w", err)
	}
	if err := deps.Docker.StartContainer(containerID); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	emitLine(progress, "  ✓ Volume and container created")

	if err := waitForPostgresReady(ctx, meta.Port, password); err != nil {
		return fmt.Errorf("cluster did not become ready: %w", err)
	}
	emitLine(progress, "  ✓ PostgreSQL ready")

	instanceDir := filepath.Join(plan.ExtractedDir, snapshotInstancesDir, meta.Name)
	dbs, found, err := readDatabaseMetadata(instanceDir)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("archive has no %s for instance %s", databaseMetadataFile, meta.Name)
	}

	roleNames, err := roleNamesFromGlobals(filepath.Join(instanceDir, "globals.sql"))
	if err != nil {
		return err
	}

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
		return err // the deferred handler records "error"
	}
	emitLine(progress, "  ✓ Roles and %d database(s) restored", restored)

	if err := deps.Store.Instances.UpdateStatus(meta.Name, "running"); err != nil {
		return fmt.Errorf("mark instance running: %w", err)
	}
	return nil
}

// markInstanceUnbuilt records that an instance exists in configuration but has
// no cluster on this host. "error" is deliberate: the instance genuinely cannot
// start (no volume, no data), and "stopped" would imply that starting it would
// work.
func markInstanceUnbuilt(st *store.Store, name string) error {
	if err := st.Instances.UpdateContainerID(name, ""); err != nil {
		return fmt.Errorf("clear stale container id for %s: %w", name, err)
	}
	if err := st.Instances.UpdateStatus(name, "error"); err != nil {
		return fmt.Errorf("mark %s unbuilt: %w", name, err)
	}
	return nil
}

// copyFile copies src to dst with the given permissions.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src) // #nosec G304 - src is inside our own extracted staging tree
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm) // #nosec G304 - dst is the daemon's data dir
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func emitLine(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

func suffixIf(value, format string, args ...any) string {
	if value == "" {
		return ""
	}
	return fmt.Sprintf(format, args...)
}

// roleNamesFromGlobals extracts the roles a globals.sql dump will create.
//
// major-upgrade gets this list by querying the live source cluster; a snapshot
// restore has no live source, so the archive itself is the only authority. The
// list feeds RestoreClusterFromArchive's post-restore verification, which
// exists because globals are replayed with ON_ERROR_STOP=0 and a silently
// failed role would otherwise go unnoticed.
//
// Mirrors captureRoleNames' exclusions: the bootstrap postgres role and pinned
// pg_* roles always exist on a fresh cluster.
func roleNamesFromGlobals(globalsPath string) ([]string, error) {
	data, err := os.ReadFile(globalsPath) // #nosec G304 - inside our own extracted staging tree
	if err != nil {
		return nil, fmt.Errorf("read globals.sql: %w", err)
	}

	var out []string
	seen := make(map[string]struct{})
	for line := range strings.SplitSeq(string(data), "\n") {
		name, ok := parseCreateRoleName(strings.TrimSpace(line))
		if !ok {
			continue
		}
		if name == "postgres" || strings.HasPrefix(name, "pg_") {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// parseCreateRoleName pulls the role name out of a `CREATE ROLE x;` statement,
// handling the quoted form pg_dumpall emits for names needing it (where an
// embedded quote is doubled).
func parseCreateRoleName(line string) (string, bool) {
	const prefix = "CREATE ROLE "
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	rest = strings.TrimSuffix(rest, ";")
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}

	if strings.HasPrefix(rest, `"`) {
		var b strings.Builder
		for i := 1; i < len(rest); i++ {
			if rest[i] != '"' {
				b.WriteByte(rest[i])
				continue
			}
			if i+1 < len(rest) && rest[i+1] == '"' { // doubled quote = literal
				b.WriteByte('"')
				i++
				continue
			}
			return b.String(), b.Len() > 0
		}
		return "", false // unterminated quote
	}

	// Unquoted: the name runs to the first whitespace.
	if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		rest = rest[:idx]
	}
	return rest, rest != ""
}

// ensureSnapshotImages makes sure every image the rebuild will need is present
// locally, pulling the missing ones. It returns the images actually pulled.
//
// Only instances carrying data are considered: configuration-only entries are
// never built, so requiring their image would refuse an otherwise valid
// recovery. Pulling is additive and reversible, which is why it belongs in the
// read-only phase rather than after oddk.db has been replaced.
func ensureSnapshotImages(ctx context.Context, params *SnapshotApplyParams, manifest *SnapshotManifest) ([]string, error) {
	return ensureImagesPresent(ctx, params.Docker, manifest.Instances, params.Progress, params.PullProgress)
}

func imageSummary(manifest *SnapshotManifest, pulled []string) string {
	seen := make(map[string]struct{})
	var images []string
	for _, entry := range manifest.Instances {
		if !entry.HasData || entry.Image == "" {
			continue
		}
		if _, dup := seen[entry.Image]; dup {
			continue
		}
		seen[entry.Image] = struct{}{}
		images = append(images, entry.Image)
	}
	if len(images) == 0 {
		return "none required"
	}
	summary := strings.Join(images, ", ")
	if len(pulled) > 0 {
		summary += fmt.Sprintf("; pulled %d", len(pulled))
	}
	return summary
}

// checkHostResources refuses sizes this host cannot honour, mirroring the
// validation 'oddk create' applies (handlers_rdbms.go -> ValidateSystemResources).
// Without it a snapshot from a larger machine builds a container with
// shared_buffers and shm_size derived from the source's RAM, and PostgreSQL
// fails to start — after the destructive phase.
func checkHostResources(instances []*InstanceMeta) error {
	for _, meta := range instances {
		if err := util.ValidateSystemResources(meta.CPUCores, meta.RAMMB); err != nil {
			return operr.Invalidf("instance %q was sized %d CPU / %d MB RAM on the source host, which this host cannot provide: %v. Recreate it with smaller resources after restoring, or apply on a larger host",
				meta.Name, meta.CPUCores, meta.RAMMB, err)
		}
	}
	return nil
}

func resourceSummary(instances []*InstanceMeta) string {
	parts := make([]string, 0, len(instances))
	for _, meta := range instances {
		parts = append(parts, fmt.Sprintf("%s=%dc/%dMB", meta.Name, meta.CPUCores, meta.RAMMB))
	}
	return strings.Join(parts, " ")
}

// checkLocaleProviders refuses databases whose collation this restore cannot
// reproduce.
//
// buildCreateDatabaseSQL documents that callers must ensure LocProvider == "c";
// major-upgrade enforces that before it touches anything and single-database
// restore falls back with a warning. The shared engine has no such guard, so
// without this an ICU or builtin-locale database would be recreated as libc and
// silently sort differently — with nothing in the output to say so.
func checkLocaleProviders(extractedDir string, instances []*InstanceMeta) error {
	for _, meta := range instances {
		dir := filepath.Join(extractedDir, snapshotInstancesDir, meta.Name)
		dbs, found, err := readDatabaseMetadata(dir)
		if err != nil {
			return fmt.Errorf("instance %s: %w", meta.Name, err)
		}
		if !found {
			continue // configuration-only instance: no databases to restore
		}
		for _, db := range dbs {
			if db.Name == "postgres" || db.LocProvider == "c" {
				continue
			}
			return operr.Invalidf("database %q on instance %q uses locale provider %q; snapshot apply only reproduces libc locales, and restoring it would silently change collation. Restore that database manually",
				db.Name, meta.Name, db.LocProvider)
		}
	}
	return nil
}
