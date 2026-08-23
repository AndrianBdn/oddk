package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/rfc3339time"
	"github.com/andrianbdn/oddk/internal/store/instances"
	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
	"github.com/andrianbdn/oddk/internal/version"
)

// Names of the fixed members of a snapshot archive.
const (
	snapshotManifestFile = "manifest.json"
	snapshotStoreFile    = "oddk.db"
	snapshotInstancesDir = "instances"

	// snapshotBasebackupDir is the per-instance directory holding a PHYSICAL
	// capture: exactly what pg_basebackup produced (base.tar.zst, pg_wal.tar,
	// backup_manifest), or base.tar.zst alone for a cold copy of a stopped
	// instance. A physical entry has no globals.sql/databases.json — the
	// cluster image carries roles, databases, ACLs and GUCs itself.
	snapshotBasebackupDir = "basebackup"
)

// Snapshot formats. "Physical" is a byte-level copy of each cluster
// (pg_basebackup, or a cold file copy for a stopped instance); "logical" is the
// portable pg_dump-based format. Physical is the default: it is cheaper to
// capture, restores byte-for-byte (per-database GUCs, database-level ACLs and
// ICU collations survive, which logical restore cannot reproduce), and a DR
// restore lands on the same image/arch anyway. Logical remains the right tool
// for cross-major, cross-architecture and single-database restores.
const (
	SnapshotFormatPhysical = "physical"
	SnapshotFormatLogical  = "logical"
)

// Capture modes for a physical entry.
const (
	captureModeBasebackup = "basebackup" // taken from a live server over the replication protocol
	captureModeCold       = "cold"       // file copy of a stopped instance's data directory
)

// NormalizeSnapshotFormat maps the wire value to a canonical format, with the
// empty string meaning "the default" (physical). Every entry point — HTTP
// handler, cron plan, CLI — funnels through this so the accepted vocabulary
// cannot drift between them.
func NormalizeSnapshotFormat(s string) (string, error) {
	switch s {
	case "", SnapshotFormatPhysical:
		return SnapshotFormatPhysical, nil
	case SnapshotFormatLogical:
		return SnapshotFormatLogical, nil
	default:
		return "", operr.Invalidf("unknown snapshot format %q (expected %q or %q)", s, SnapshotFormatPhysical, SnapshotFormatLogical)
	}
}

// SnapshotFilePrefix begins the filename of every snapshot archive, and
// SnapshotStagingPrefix every in-progress staging directory. Both are exported
// because the daemon's startup sweep needs to recognise them: staging dirs are
// orphans to delete, finished snapshots are deliberately unreferenced by
// backup_history and must not be reported as stray archives.
const (
	SnapshotFilePrefix    = "snapshot-"
	SnapshotStagingPrefix = ".snapshot-"
)

// Snapshot archive format versions. `snapshot apply` refuses anything newer
// than SnapshotFormatVersion, which is what stops a pre-physical binary from
// hunting for globals.sql inside a physical archive and failing confusingly.
//
// A LOGICAL snapshot still stamps v1 because its layout genuinely is v1 — the
// version describes the archive, not the binary that wrote it. NOTE this does
// NOT make new logical archives applyable by older binaries: the pre-existing
// OddkVersion gate refuses any archive created by a newer oddk regardless of
// format. --logical's real benefits are portability across architectures and
// single-database workflows, not older readers.
const (
	snapshotFormatVersionLogical  = 1
	snapshotFormatVersionPhysical = 2

	// SnapshotFormatVersion is the newest archive format this binary
	// understands. Bump it only for a change an older reader cannot cope with.
	SnapshotFormatVersion = snapshotFormatVersionPhysical
)

// SnapshotManifest is the first entry in a snapshot archive, so a reader can
// check compatibility from the first few kilobytes rather than streaming
// through every dump to find it.
type SnapshotManifest struct {
	FormatVersion int       `json:"formatVersion"`
	OddkVersion   string    `json:"oddkVersion"`
	CreatedAt     time.Time `json:"createdAt"`
	SourceHost    string    `json:"sourceHost"`

	// SourceArch is runtime.GOARCH on the capturing host. Physical data
	// directories are only supported on the same platform, so apply and
	// restore-instance refuse a physical entry on a different architecture
	// (logical archives restore anywhere; the field is informational there).
	// Empty in pre-0.1.61 archives, which are all logical anyway.
	SourceArch string `json:"sourceArch,omitempty"`

	// Migrations is the applied-migration list from the source's oddk.db. It
	// describes the embedded store's schema more precisely than OddkVersion.
	Migrations []string `json:"migrations"`

	Instances []SnapshotInstanceEntry `json:"instances"`
}

// SnapshotInstanceEntry records what the snapshot holds for one instance.
type SnapshotInstanceEntry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Image   string `json:"image"`

	// HasData is false for a configuration-only entry: the instance's
	// configuration was captured but its databases were not.
	HasData bool `json:"hasData"`

	// Format says how this entry's data was captured: "physical"
	// (pg_basebackup / cold copy under basebackup/) or "logical"
	// (globals.sql + databases/). Empty means logical — pre-0.1.61 archives
	// predate the field. Restore paths branch on this, never on a flag.
	Format string `json:"format,omitempty"`

	// CaptureMode distinguishes physical captures: "basebackup" was taken from
	// the live server, "cold" is a file copy of a stopped instance's data dir.
	// A cold entry is restored to a STOPPED instance — the deployment's shape
	// is reproduced, not just its bytes.
	CaptureMode string `json:"captureMode,omitempty"`

	// SkipReason explains a configuration-only entry, and is shown by apply so
	// the operator is never surprised by an instance coming back empty.
	SkipReason string `json:"skipReason,omitempty"`
}

// entryFormat is the format of one entry, treating the empty string (pre-0.1.61
// archives) as logical. Always read the format through this.
func entryFormat(entry SnapshotInstanceEntry) string {
	if entry.Format == SnapshotFormatPhysical {
		return SnapshotFormatPhysical
	}
	return SnapshotFormatLogical
}

// MakeSnapshotParams configures a snapshot.
type MakeSnapshotParams struct {
	BackupDir string // where the snapshot archive is written
	Comment   string // free-text note stored with the catalogue record

	// Format is SnapshotFormatPhysical or SnapshotFormatLogical; empty means
	// physical (callers should have gone through NormalizeSnapshotFormat).
	Format string

	// SpreadCheckpoint selects pg_basebackup's spread checkpoint, which paces
	// the initial checkpoint over minutes instead of spiking I/O. Scheduled
	// runs set it; interactive runs use a fast checkpoint because an operator
	// is waiting.
	SpreadCheckpoint bool
}

// SnapshotCaptureFailure is one instance whose data capture was ATTEMPTED and
// FAILED, as opposed to one that had nothing to capture (no container, stopped
// under --logical, paused). Both end up configuration-only in the archive; only
// this one means something is broken and needs fixing.
type SnapshotCaptureFailure struct {
	Instance string `json:"instance"`
	Stage    string `json:"stage"` // "base backup", "cold copy", "dump", "decrypt password"
	Error    string `json:"error"`
}

// MakeSnapshotResult describes the produced archive.
type MakeSnapshotResult struct {
	ID                int                     `json:"id,omitempty"`
	Path              string                  `json:"path"`
	Size              int64                   `json:"size"`
	Timestamp         time.Time               `json:"timestamp"`
	Format            string                  `json:"format"`
	Instances         []SnapshotInstanceEntry `json:"instances"`
	InstancesWithData int                     `json:"instancesWithData"`
	ConfigOnly        int                     `json:"configOnly"`

	// CaptureFailures is non-empty when the archive was written but is NOT a
	// complete capture of the deployment. Callers must treat that as a failed
	// run — see CaptureFailureError — while still keeping, shipping and
	// cataloguing the archive.
	CaptureFailures []SnapshotCaptureFailure `json:"captureFailures,omitempty"`
}

// CaptureFailureError summarises the per-instance capture failures as one
// error, or nil when the capture was complete. It is what turns a degraded
// archive into a reported failure at every layer above this one.
func (r *MakeSnapshotResult) CaptureFailureError() error {
	if len(r.CaptureFailures) == 0 {
		return nil
	}
	parts := make([]string, 0, len(r.CaptureFailures))
	for _, f := range r.CaptureFailures {
		parts = append(parts, fmt.Sprintf("%s (%s: %s)", f.Instance, f.Stage, f.Error))
	}
	return fmt.Errorf("the archive was written but %d of %d instance(s) could not be captured and hold NO database contents in it: %s",
		len(r.CaptureFailures), len(r.Instances), strings.Join(parts, "; "))
}

// MakeSnapshot captures the whole deployment — every instance's databases and
// roles, plus the control plane's own oddk.db — into a single archive that
// `snapshot apply` can rebuild a host from.
//
// A stopped instance is captured configuration-only rather than failing the
// whole snapshot: its data cannot be dumped without a live server, but letting
// one stopped instance block disaster-recovery capture for every other instance
// would be the wrong trade. Such entries are marked in the manifest and warned
// about, so they cannot quietly look like a complete capture.
//
// Instances are dumped sequentially, so a snapshot is NOT a single point in
// time across instances. That is acceptable for host moves and DR, and must be
// stated wherever this is documented for users.
func MakeSnapshot(ctx context.Context, deps *Dependencies, params *MakeSnapshotParams) (*MakeSnapshotResult, error) {
	if _, err := os.Stat(params.BackupDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("backup directory does not exist: %s", params.BackupDir)
	}
	format, err := NormalizeSnapshotFormat(params.Format)
	if err != nil {
		return nil, err
	}

	timestamp := time.Now().UTC()
	timestampStr := timestamp.Format("20060102150405")
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	host = sanitizeHostForFilename(host)

	snapshotName := fmt.Sprintf("%s%s-%s", SnapshotFilePrefix, host, timestampStr)
	archivePath := filepath.Join(params.BackupDir, snapshotName+".tar.zst")
	stagingDir := filepath.Join(params.BackupDir, SnapshotStagingPrefix+timestampStr)

	if _, err := os.Stat(archivePath); err == nil {
		return nil, fmt.Errorf("snapshot already exists: %s", archivePath)
	}
	if err := os.MkdirAll(stagingDir, 0o750); err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()

	entries, captureFailures, err := stageAllInstances(ctx, deps, stagingDir, format, params.SpreadCheckpoint)
	if err != nil {
		return nil, err
	}

	// Copy the control plane's own state. VACUUM INTO is consistent against a
	// live database, so the daemon keeps running throughout.
	if err := deps.Store.VacuumInto(filepath.Join(stagingDir, snapshotStoreFile)); err != nil {
		return nil, fmt.Errorf("copy oddk.db: %w", err)
	}

	migrations, err := deps.Store.AppliedMigrations()
	if err != nil {
		return nil, err
	}

	// The version describes the archive's layout: logical archives are still
	// v1 (see the constants above for why that does not imply older readers
	// can apply them).
	formatVersion := snapshotFormatVersionLogical
	if format == SnapshotFormatPhysical {
		formatVersion = snapshotFormatVersionPhysical
	}
	manifest := &SnapshotManifest{
		FormatVersion: formatVersion,
		OddkVersion:   version.Version,
		CreatedAt:     timestamp,
		SourceHost:    host,
		SourceArch:    runtime.GOARCH,
		Migrations:    migrations,
		Instances:     entries,
	}
	if err := writeSnapshotManifest(stagingDir, manifest); err != nil {
		return nil, err
	}

	// Manifest first, then the store, then the bulky dumps — see
	// CreateTarZstdOrdered for why the order matters.
	archiveEntries := []compression.ArchiveEntry{
		{SourcePath: filepath.Join(stagingDir, snapshotManifestFile), ArchiveName: snapshotManifestFile},
		{SourcePath: filepath.Join(stagingDir, snapshotStoreFile), ArchiveName: snapshotStoreFile},
	}
	instancesPath := filepath.Join(stagingDir, snapshotInstancesDir)
	if _, err := os.Stat(instancesPath); err == nil {
		archiveEntries = append(archiveEntries, compression.ArchiveEntry{
			SourcePath: instancesPath, ArchiveName: snapshotInstancesDir,
		})
	}

	// The archive is read back before it is published, and this asserts it holds
	// what the manifest promises. On any failure nothing is written to
	// archivePath and nothing is catalogued — see writeVerifiedArchive.
	size, err := compression.NewCompressor().CreateTarZstdOrdered(ctx, archiveEntries, archivePath,
		func(members []compression.Member) error {
			return assertSnapshotMembers(members, entries)
		})
	if err != nil {
		return nil, fmt.Errorf("create snapshot archive: %w", err)
	}

	withData := 0
	for _, e := range entries {
		if e.HasData {
			withData++
		}
	}

	// Catalogue the snapshot only NOW, after the archive exists.
	//
	// The ordering is the whole answer to the self-reference problem: VacuumInto
	// above copied oddk.db into the archive, so a record inserted before that
	// point would be captured mid-flight and every restore of this archive would
	// carry a permanently unfinished row describing the archive it came from.
	// Recording afterwards means the embedded copy simply has no row for it,
	// which is correct — a snapshot is an INPUT to a restored host, not
	// something that host produced. Provenance lives in manifest.json, outside
	// the SQLite copy, where it cannot self-reference.
	// The per-instance list (not just the counts) goes into the catalogue so
	// the checklist can answer "is THIS instance's data in the newest
	// snapshot?" — a configuration-only entry must not read as coverage.
	recorded := make([]snapshotstore.RecordInstance, 0, len(entries))
	for _, e := range entries {
		recorded = append(recorded, snapshotstore.RecordInstance{Name: e.Name, HasData: e.HasData})
	}
	record := &snapshotstore.Record{
		Filename:          filepath.Base(archivePath),
		CreatedAt:         rfc3339time.Time{Time: timestamp},
		Size:              size,
		Status:            "completed",
		Format:            format,
		InstancesWithData: withData,
		ConfigOnly:        len(entries) - withData,
		Instances:         recorded,
		LocalPath:         archivePath,
		CommentStr:        params.Comment,
	}
	if err := deps.Store.Snapshot.RecordSnapshot(record); err != nil {
		// The archive is on disk and usable; failing the whole operation would
		// discard a good snapshot over a bookkeeping error. Surface it loudly
		// instead — retention and upload work off the catalogue, so an
		// unrecorded snapshot will not be pruned or shipped.
		log.Printf("WARNING: snapshot %s was created but could not be recorded in the catalogue: %v", archivePath, err)
	}

	return &MakeSnapshotResult{
		ID:                record.ID,
		Path:              archivePath,
		Size:              size,
		Timestamp:         timestamp,
		Format:            format,
		Instances:         entries,
		InstancesWithData: withData,
		ConfigOnly:        len(entries) - withData,
		CaptureFailures:   captureFailures,
	}, nil
}

// captureAction is how stageAllInstances should capture one instance.
type captureAction int

const (
	captureConfigOnly captureAction = iota
	captureBasebackup
	captureCold
	captureLogicalDump
)

// decideCapture chooses how to capture one instance, and returns the reason
// when the answer is configuration-only.
//
// It dispatches on actualState — what Docker reports the container is doing
// right now — and NEVER on storedStatus. That distinction is the difference
// between a correct DR archive and a silently empty one, because storedStatus
// drifts, in ways this codebase creates itself:
//
//   - startup reconcile repairs only a few (storedStatus, dockerState) pairs,
//     so a crashed switch/upgrade/reconfigure leaves its transient status
//     behind forever;
//   - ConsistencyCheckOp can latch "broken-port" onto an instance that is up
//     and serving, from an ordinary `oddk list` during a blip, and never
//     clears it;
//   - an operator can `docker start`/`docker stop` out of band.
//
// Dispatching on that string reduced a live, serving cluster to
// configuration-only in EVERY subsequent archive — the worst failure this
// command had, because it failed OPEN: the snapshot still completed, was
// catalogued, was uploaded, and counted toward the retention floor.
//
// storedStatus survives only as warning text, and as the caller's rule for
// whether a capture failure may abort the whole run.
//
// The third return value separates the two kinds of configuration-only outcome,
// because they deserve opposite reporting:
//
//   - ANOMALOUS (true): the instance's data almost certainly still exists on
//     this host and we could not read it — its container vanished, its state
//     could not be determined, or it is paused/restarting. ODDK's data lives in
//     a named volume (oddk-data-<instance>), which `docker rm -f` does NOT
//     remove, so "the container is gone" does not mean "the data is gone": it
//     means a recoverable cluster was sitting right there and the archive was
//     written without it. That is a capture failure in everything but name, and
//     reporting the run green over it recreates — one level up — exactly the bug
//     that made capture dispatch on the stored status.
//
//   - EXPECTED (false): there is nothing to capture, or the operator chose a
//     format that cannot capture it. An instance with no container at all is
//     already broken and holds no cluster to read; a cleanly stopped instance
//     under --logical is a documented limitation of that format (physical
//     cold-copies it instead, which is one reason physical is the default).
//     These recur every run for as long as the condition lasts, so alerting on
//     them would teach operators to filter the channel — the exact outcome the
//     one-notification-per-run rule exists to avoid.
//
// Both still produce a configuration-only entry with hasData:false, a
// skipReason, a stdout warning and a "✗ config-only" verdict on the checklist.
// The flag decides only whether the run is additionally reported as FAILED.
func decideCapture(format string, storedStatus instances.InstanceStatus, containerID, actualState string, stateErr error) (action captureAction, reason string, anomalous bool) {
	switch {
	case containerID == "":
		return captureConfigOnly, fmt.Sprintf("instance is recorded %q and has no container; databases not captured", storedStatus), false
	case stateErr != nil:
		return captureConfigOnly, fmt.Sprintf("container state could not be determined (%v); databases not captured", stateErr), true
	case actualState == "not found":
		return captureConfigOnly, fmt.Sprintf("instance is recorded %q and its container no longer exists; databases not captured", storedStatus), true
	}

	if format == SnapshotFormatPhysical {
		switch actualState {
		case "running":
			return captureBasebackup, "", false
		case "stopped":
			// GetContainerStatus normalizes every existing, non-live state
			// (exited/created/dead) to "stopped" — exactly the set that is safe
			// to copy file-by-file. A stopped cluster's data directory is a
			// valid physical backup (worst case it recovers like a crash on
			// start), which is why physical mode captures it rather than
			// reducing it to configuration the way logical mode must.
			return captureCold, "", false
		default:
			// paused/restarting: neither cleanly stopped (a cold copy would
			// tear) nor serving (a basebackup would hang). The cluster itself is
			// intact and will be capturable again as soon as the container
			// settles, so leaving it out of the archive is a real gap, not a
			// steady state to be tolerated quietly.
			return captureConfigOnly, fmt.Sprintf("container is %q, which is neither serving nor cleanly stopped; databases not captured", actualState), true
		}
	}

	// Logical mode needs a live server: a dump cannot read a stopped cluster.
	// This is the format's documented limitation rather than something going
	// wrong, so it does not fail the run — an instance parked for weeks would
	// otherwise fail every scheduled run for weeks. Physical mode cold-copies
	// the same instance, which is why it is the default.
	if actualState != "running" {
		return captureConfigOnly, fmt.Sprintf("container is %q and logical dumps need a live server; databases not captured", actualState), false
	}
	return captureLogicalDump, "", false
}

// stageAllInstances captures every instance into stagingDir/instances/<name>/,
// returning one manifest entry per instance.
//
// Dispatch is on the container's ACTUAL Docker state, never on the instance's
// stored status — see the long comment in the loop for why that distinction is
// the difference between a correct DR archive and a silently empty one.
//
// Logical mode dumps whatever is running and captures the rest
// configuration-only. Physical mode does better on a stopped container: a
// stopped cluster's data directory is a valid physical backup (worst case it
// recovers like a crash on start), so it is COLD-COPIED rather than reduced to
// configuration — the whole reason a deployment stops an instance is that its
// data still matters. Only an instance with no container at all, one whose
// container state cannot be read, and one that is paused/restarting (neither
// serving nor cleanly stopped) stay configuration-only.
//
// A capture that is ATTEMPTED and FAILS degrades that one entry to
// configuration-only and is reported through the returned failure list; it does
// not abort the run. See the comment on degrade below for why that is the safer
// of the two options, and what still does abort.
func stageAllInstances(ctx context.Context, deps *Dependencies, stagingDir, format string, spreadCheckpoint bool) ([]SnapshotInstanceEntry, []SnapshotCaptureFailure, error) {
	list, err := deps.Store.Instances.List()
	if err != nil {
		return nil, nil, fmt.Errorf("list instances: %w", err)
	}

	entries := make([]SnapshotInstanceEntry, 0, len(list))
	var failures []SnapshotCaptureFailure
	for i := range list {
		instance := &list[i]
		entry := SnapshotInstanceEntry{
			Name:    instance.Name,
			Version: instance.Version,
			Image:   instance.Image,
			Format:  format,
		}

		instanceDir := filepath.Join(stagingDir, snapshotInstancesDir, instance.Name)
		if err := os.MkdirAll(instanceDir, 0o750); err != nil {
			return nil, nil, fmt.Errorf("create staging dir for %s: %w", instance.Name, err)
		}
		// Every entry carries instance.json: apply rebuilds the container from
		// it whether or not the entry holds data. The logical dump path writes
		// its own copy inside stageInstanceDump (shared with per-instance
		// backups); every other branch needs it written here.
		writeMeta := func() error {
			return writeInstanceMetadata(instanceDir, captureInstanceMetadata(deps, instance))
		}

		configOnly := func(reason string) {
			entry.SkipReason = reason
			log.Printf("WARNING: snapshot: instance %q - capturing configuration only, NOT its databases (%s)",
				instance.Name, reason)
			entries = append(entries, entry)
		}

		// Ask Docker what the container is doing RIGHT NOW — never the stored
		// status. See decideCapture for why.
		var actual string
		var stateErr error
		if instance.ContainerID != "" {
			actual, stateErr = deps.Docker.GetContainerStatus(instance.ContainerID)
		}

		action, reason, anomalous := decideCapture(format, instance.Status, instance.ContainerID, actual, stateErr)

		// The capture is correct either way, but a disagreement means the stored
		// row is wrong and something should be fixed — say so.
		if stateErr == nil && actual != "" && string(instance.Status) != actual {
			log.Printf("WARNING: snapshot: instance %q is recorded %q but its container is %q; capturing from the container's ACTUAL state (the stored status is stale — 'oddk instance start %s' will correct it)",
				instance.Name, instance.Status, actual, instance.Name)
		}

		// A capture failure DEGRADES this one entry to configuration-only. It does
		// not abort the run.
		//
		// The original behaviour was the opposite: any genuine capture failure
		// returned an error and produced no archive for ANY instance. That trades
		// one instance's bad night for every instance's — and the scheduler dedups
		// on the slot start, so the interval is burned too. A persistent trigger
		// (exhausted WAL senders, wal_level=minimal, a tablespace) therefore meant
		// weeks with no DR archive at all for a deployment that was otherwise
		// perfectly capturable.
		//
		// Degrading is only defensible because the failure is now impossible to
		// miss. It is reported six ways: the manifest entry (hasData:false plus a
		// specific skipReason), snapshot_history.instances_json — which makes
		// `oddk checklist` print "✗ config-only" for that instance — a stdout
		// WARNING, the capture phase of the scheduled run recorded as FAILED, the
		// notification that phase sends, and a non-zero exit from
		// `oddk snapshot make`. Before those existed (v0.1.62 and v0.1.67),
		// aborting was the safer of two bad options; now it is just the worse one.
		//
		// What still aborts is anything that makes the ARCHIVE ITSELF
		// untrustworthy: listing instances, staging directories, oddk.db, the
		// manifest, and the archive write with its read-back verification.
		degrade := func(what string, cause error) {
			failures = append(failures, SnapshotCaptureFailure{
				Instance: instance.Name, Stage: what, Error: cause.Error(),
			})
			resetToConfigOnly(instanceDir, writeMeta)
			configOnly(fmt.Sprintf("%s FAILED (%v); databases not captured — this instance would restore EMPTY",
				what, cause))
		}

		// Every entry carries instance.json. The logical dump path writes its
		// own copy inside stageInstanceDump (shared with per-instance backups).
		if action != captureLogicalDump {
			if err := writeMeta(); err != nil {
				return nil, nil, fmt.Errorf("stage %s: %w", instance.Name, err)
			}
		}

		switch action {
		case captureConfigOnly:
			// An ANOMALOUS skip is a capture failure even though nothing was
			// attempted: the cluster is still on this host (ODDK keeps data in a
			// named volume, which removing a container does not delete) and the
			// archive is being written without it. Recording it here is what
			// gives it the non-zero exit, the failed capture phase and the
			// notification — the same treatment a failed base backup gets, for
			// the same reason: after a restore, both leave that instance EMPTY.
			// Expected skips (no container at all, a stopped instance under
			// --logical) stay quiet; see decideCapture.
			if anomalous {
				failures = append(failures, SnapshotCaptureFailure{
					Instance: instance.Name, Stage: "capture", Error: reason,
				})
			}
			configOnly(reason)

		case captureBasebackup:
			password, err := crypto.DecryptPassword(instance.Password, deps.MasterKey)
			if err != nil {
				degrade("decrypt password", err)
				continue
			}
			log.Printf("Snapshot: base backup of instance %s", instance.Name)
			if err := stagePhysicalBasebackup(ctx, deps, instance, password, instanceDir, spreadCheckpoint); err != nil {
				degrade("base backup", err)
				continue
			}
			entry.HasData = true
			entry.CaptureMode = captureModeBasebackup
			entries = append(entries, entry)

		case captureCold:
			log.Printf("Snapshot: cold copy of stopped instance %s", instance.Name)
			if err := stagePhysicalCold(ctx, deps, instance, instanceDir); err != nil {
				degrade("cold copy", err)
				continue
			}
			entry.HasData = true
			entry.CaptureMode = captureModeCold
			entries = append(entries, entry)

		case captureLogicalDump:
			password, err := crypto.DecryptPassword(instance.Password, deps.MasterKey)
			if err != nil {
				degrade("decrypt password", err)
				continue
			}
			log.Printf("Snapshot: dumping instance %s", instance.Name)
			if err := stageInstanceDump(ctx, deps, instance, password, instanceDir); err != nil {
				degrade("dump", err)
				continue
			}
			entry.HasData = true
			entries = append(entries, entry)
		}
	}

	return entries, failures, nil
}

// resetToConfigOnly discards whatever a failed capture left in the staging
// directory and makes sure the entry still carries its instance.json.
//
// A capture that fails part-way through has usually already written something —
// a truncated base.tar.zst, a partial dump directory. Archiving that would put
// bytes into every copy of the snapshot that no restore path will ever read
// (apply and restore-instance branch on the manifest entry, which now says
// hasData:false), and would leave a trap for anyone who inspects the files
// instead of the manifest and concludes the data is in there.
//
// Failures here are logged rather than fatal: a missing instance.json is caught
// by assertSnapshotMembers before the archive is published, which is the check
// that must have the last word anyway.
func resetToConfigOnly(instanceDir string, writeMeta func() error) {
	// Everything a capture can produce under the instance directory; instance.json
	// is deliberately not in the list.
	for _, leftover := range []string{snapshotBasebackupDir, "databases", "globals.sql", databaseMetadataFile} {
		if err := os.RemoveAll(filepath.Join(instanceDir, leftover)); err != nil {
			log.Printf("Warning: snapshot: could not discard partial %q left by a failed capture: %v", leftover, err)
		}
	}
	if err := writeMeta(); err != nil {
		log.Printf("Warning: snapshot: could not write %s for a degraded entry: %v", instanceMetadataFile, err)
	}
}

// sanitizeHostForFilename reduces a hostname to characters that are safe in a
// filename. Real hostnames already are, but the value comes from the OS and
// ends up in a path, so it is not taken on trust.
func sanitizeHostForFilename(host string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, host)
	cleaned = strings.Trim(cleaned, ".-")
	if cleaned == "" {
		return "unknown"
	}
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	return cleaned
}

func writeSnapshotManifest(dir string, manifest *SnapshotManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshotManifestFile), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", snapshotManifestFile, err)
	}
	return nil
}

// ReadSnapshotManifest reads manifest.json from an extracted snapshot
// directory. Unlike the per-instance metadata readers, absence is an error:
// every snapshot has one, so a missing manifest means the archive is not a
// snapshot (or is truncated).
func ReadSnapshotManifest(extractedDir string) (*SnapshotManifest, error) {
	path := filepath.Join(extractedDir, snapshotManifestFile)
	data, err := os.ReadFile(path) // #nosec G304 - path is the daemon's own extracted snapshot directory
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", snapshotManifestFile, err)
	}
	var manifest SnapshotManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse %s: %w", snapshotManifestFile, err)
	}
	return &manifest, nil
}

// assertSnapshotMembers checks that a freshly written snapshot archive actually
// holds what its manifest promises.
//
// This is the structural half of verification, and it is the defensible version
// of a "size baseline". A byte threshold cannot do this job: a perfectly good
// snapshot is a few megabytes for one small cluster, or a few kilobytes when
// every instance was captured configuration-only, and for physical archives the
// WAL term alone swings by orders of magnitude between an idle and a busy
// capture window. What an operator actually wants to know is "did every instance
// that claims to have data actually get its data written", and that is a
// presence-and-non-zero question, not a size question.
//
// The expected set is derived from the same `entries` slice that becomes the
// manifest and instances_json, never from hard-coded paths, so the assertion and
// the archive layout cannot drift apart.
func assertSnapshotMembers(members []compression.Member, entries []SnapshotInstanceEntry) error {
	// A zero-length file decompresses cleanly with zero members, so the stream
	// check alone cannot catch it. This is the branch that does.
	if len(members) == 0 {
		return fmt.Errorf("archive contains no members")
	}

	byName := make(map[string]compression.Member, len(members))
	var firstFile string
	for _, m := range members {
		byName[m.Name] = m
		if firstFile == "" && !m.IsDir {
			firstFile = m.Name
		}
	}

	// Ordering is a load-bearing property, not a cosmetic one: apply reads the
	// manifest to check version compatibility, and tar has no index, so a
	// manifest that is not first means streaming gigabytes to find it. Asserting
	// it on the ARTIFACT (rather than only in TestSnapshotMake) means an archive
	// with the wrong order can never be published.
	if firstFile != snapshotManifestFile {
		return fmt.Errorf("first archive member is %q, expected %q", firstFile, snapshotManifestFile)
	}

	nonEmpty := func(name string) error {
		m, ok := byName[name]
		if !ok {
			return fmt.Errorf("archive is missing %s", name)
		}
		if m.Size == 0 {
			return fmt.Errorf("archive member %s is empty", name)
		}
		return nil
	}

	if err := nonEmpty(snapshotStoreFile); err != nil {
		return err
	}

	for _, e := range entries {
		base := snapshotInstancesDir + "/" + e.Name

		// Every entry carries its configuration, including configuration-only
		// ones — apply rebuilds the container from it either way.
		if err := nonEmpty(base + "/" + instanceMetadataFile); err != nil {
			return fmt.Errorf("instance %s: %w", e.Name, err)
		}

		if !e.HasData {
			continue
		}

		// An entry claiming data must actually carry a non-empty payload. This is
		// the check that catches "the dump ran, returned success, and produced
		// nothing" — the failure mode that otherwise reaches a restore.
		if entryFormat(e) == SnapshotFormatPhysical {
			// PG < 15 cannot --compress=client-zstd, so live captures stage a
			// plain base.tar; restore already accepts either via physicalBasePath.
			zst := base + "/" + snapshotBasebackupDir + "/" + physicalBaseTarZst
			plain := base + "/" + snapshotBasebackupDir + "/" + physicalBaseTar
			if nonEmpty(zst) != nil && nonEmpty(plain) != nil {
				return fmt.Errorf("instance %s claims physical data: archive has neither %s nor %s",
					e.Name, physicalBaseTarZst, physicalBaseTar)
			}
			continue
		}

		if err := nonEmpty(base + "/globals.sql"); err != nil {
			return fmt.Errorf("instance %s claims logical data: %w", e.Name, err)
		}
		hasDatabasePayload := false
		for name, m := range byName {
			if !m.IsDir && m.Size > 0 && strings.HasPrefix(name, base+"/databases/") {
				hasDatabasePayload = true
				break
			}
		}
		if !hasDatabasePayload {
			return fmt.Errorf("instance %s claims logical data but no non-empty file exists under %s/databases/", e.Name, base)
		}
	}

	return nil
}
