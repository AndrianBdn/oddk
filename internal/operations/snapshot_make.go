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

	entries, err := stageAllInstances(ctx, deps, stagingDir, format, params.SpreadCheckpoint)
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

	size, err := compression.NewCompressor().CreateTarZstdOrdered(ctx, archiveEntries, archivePath)
	if err != nil {
		_ = os.Remove(archivePath)
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
	}, nil
}

// stageAllInstances captures every instance into stagingDir/instances/<name>/,
// returning one manifest entry per instance.
//
// Logical mode dumps running instances and captures the rest
// configuration-only. Physical mode does better on stopped instances: a
// stopped cluster's data directory is a valid physical backup (worst case it
// recovers like a crash on start), so it is COLD-COPIED rather than reduced to
// configuration — the whole reason a deployment stops an instance is that its
// data still matters. Only "error" instances (nothing reliable to copy) and
// stopped instances whose container is gone stay configuration-only.
func stageAllInstances(ctx context.Context, deps *Dependencies, stagingDir, format string, spreadCheckpoint bool) ([]SnapshotInstanceEntry, error) {
	list, err := deps.Store.Instances.List()
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}

	entries := make([]SnapshotInstanceEntry, 0, len(list))
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
			return nil, fmt.Errorf("create staging dir for %s: %w", instance.Name, err)
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

		switch {
		case format == SnapshotFormatPhysical && instance.Status == "running":
			if err := writeMeta(); err != nil {
				return nil, fmt.Errorf("stage %s: %w", instance.Name, err)
			}
			password, err := crypto.DecryptPassword(instance.Password, deps.MasterKey)
			if err != nil {
				return nil, fmt.Errorf("decrypt password for %s: %w", instance.Name, err)
			}
			log.Printf("Snapshot: base backup of instance %s", instance.Name)
			if err := stagePhysicalBasebackup(ctx, deps, instance, password, instanceDir, spreadCheckpoint); err != nil {
				return nil, fmt.Errorf("base backup of instance %s: %w", instance.Name, err)
			}
			entry.HasData = true
			entry.CaptureMode = captureModeBasebackup
			entries = append(entries, entry)

		case format == SnapshotFormatPhysical && instance.Status == "stopped" && instance.ContainerID != "":
			if err := writeMeta(); err != nil {
				return nil, fmt.Errorf("stage %s: %w", instance.Name, err)
			}
			// The stored status can drift from Docker between daemon restarts
			// (reconcile runs only at startup): an operator can `docker start`
			// or `docker rm` the container out-of-band. Dispatch on the
			// container's ACTUAL state — a cold copy of a live cluster would be
			// silently torn, and a vanished container must degrade this one
			// instance rather than abort the whole DR capture.
			actual, statusErr := deps.Docker.GetContainerStatus(instance.ContainerID)
			switch {
			case statusErr != nil:
				configOnly(fmt.Sprintf("instance is stopped but its container state could not be determined (%v); databases not captured", statusErr))
			case actual == "not found":
				configOnly("instance is stopped and its container no longer exists; databases not captured")
			case actual == "running":
				// The server is actually up: capture it the safe way, over the
				// replication protocol, rather than refusing the whole night.
				log.Printf("WARNING: snapshot: instance %q is recorded stopped but its container is RUNNING (started outside ODDK?); capturing with pg_basebackup instead of a cold copy", instance.Name)
				password, err := crypto.DecryptPassword(instance.Password, deps.MasterKey)
				if err != nil {
					return nil, fmt.Errorf("decrypt password for %s: %w", instance.Name, err)
				}
				if err := stagePhysicalBasebackup(ctx, deps, instance, password, instanceDir, spreadCheckpoint); err != nil {
					return nil, fmt.Errorf("base backup of instance %s: %w", instance.Name, err)
				}
				entry.HasData = true
				entry.CaptureMode = captureModeBasebackup
				entries = append(entries, entry)
			case actual == "stopped":
				// GetContainerStatus normalizes every existing, non-live state
				// (exited/created/dead) to "stopped" — exactly the set that is
				// safe to copy file-by-file.
				log.Printf("Snapshot: cold copy of stopped instance %s", instance.Name)
				if err := stagePhysicalCold(ctx, deps, instance, instanceDir); err != nil {
					return nil, fmt.Errorf("cold copy of instance %s: %w", instance.Name, err)
				}
				entry.HasData = true
				entry.CaptureMode = captureModeCold
				entries = append(entries, entry)
			default:
				// paused/restarting: neither cleanly stopped (cold copy would
				// tear) nor serving (basebackup would hang).
				configOnly(fmt.Sprintf("instance is recorded stopped but its container is %q; databases not captured", actual))
			}

		case format == SnapshotFormatPhysical:
			if err := writeMeta(); err != nil {
				return nil, fmt.Errorf("stage %s: %w", instance.Name, err)
			}
			configOnly(fmt.Sprintf("instance was %s at snapshot time; databases not captured", instance.Status))

		case instance.Status != "running":
			// Logical mode cannot capture a stopped instance at all: dumps need
			// a live server.
			if err := writeMeta(); err != nil {
				return nil, fmt.Errorf("stage %s: %w", instance.Name, err)
			}
			configOnly(fmt.Sprintf("instance was %s at snapshot time; databases not captured", instance.Status))

		default:
			password, err := crypto.DecryptPassword(instance.Password, deps.MasterKey)
			if err != nil {
				return nil, fmt.Errorf("decrypt password for %s: %w", instance.Name, err)
			}
			log.Printf("Snapshot: dumping instance %s", instance.Name)
			if err := stageInstanceDump(ctx, deps, instance, password, instanceDir); err != nil {
				return nil, fmt.Errorf("dump instance %s: %w", instance.Name, err)
			}
			entry.HasData = true
			entries = append(entries, entry)
		}
	}

	return entries, nil
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
