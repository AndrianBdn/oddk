package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/rfc3339time"
	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
	"github.com/andrianbdn/oddk/internal/version"
)

// Names of the fixed members of a snapshot archive.
const (
	snapshotManifestFile = "manifest.json"
	snapshotStoreFile    = "oddk.db"
	snapshotInstancesDir = "instances"
)

// SnapshotFilePrefix begins the filename of every snapshot archive, and
// SnapshotStagingPrefix every in-progress staging directory. Both are exported
// because the daemon's startup sweep needs to recognise them: staging dirs are
// orphans to delete, finished snapshots are deliberately unreferenced by
// backup_history and must not be reported as stray archives.
const (
	SnapshotFilePrefix    = "snapshot-"
	SnapshotStagingPrefix = ".snapshot-"
)

// SnapshotFormatVersion identifies the archive layout. Bump it only for a
// change a older reader cannot cope with; `snapshot apply` refuses anything
// newer than it understands.
const SnapshotFormatVersion = 1

// SnapshotManifest is the first entry in a snapshot archive, so a reader can
// check compatibility from the first few kilobytes rather than streaming
// through every dump to find it.
type SnapshotManifest struct {
	FormatVersion int       `json:"formatVersion"`
	OddkVersion   string    `json:"oddkVersion"`
	CreatedAt     time.Time `json:"createdAt"`
	SourceHost    string    `json:"sourceHost"`

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

	// HasData is false for a configuration-only entry: the instance was not
	// running, so its configuration was captured but its databases were not.
	HasData bool `json:"hasData"`

	// SkipReason explains a configuration-only entry, and is shown by apply so
	// the operator is never surprised by an instance coming back empty.
	SkipReason string `json:"skipReason,omitempty"`
}

// MakeSnapshotParams configures a snapshot.
type MakeSnapshotParams struct {
	BackupDir string // where the snapshot archive is written
	Comment   string // free-text note stored with the catalogue record
}

// MakeSnapshotResult describes the produced archive.
type MakeSnapshotResult struct {
	ID                int                     `json:"id,omitempty"`
	Path              string                  `json:"path"`
	Size              int64                   `json:"size"`
	Timestamp         time.Time               `json:"timestamp"`
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

	entries, err := stageAllInstances(ctx, deps, stagingDir)
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

	manifest := &SnapshotManifest{
		FormatVersion: SnapshotFormatVersion,
		OddkVersion:   version.Version,
		CreatedAt:     timestamp,
		SourceHost:    host,
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
	record := &snapshotstore.Record{
		Filename:          filepath.Base(archivePath),
		CreatedAt:         rfc3339time.Time{Time: timestamp},
		Size:              size,
		Status:            "completed",
		InstancesWithData: withData,
		ConfigOnly:        len(entries) - withData,
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
		Instances:         entries,
		InstancesWithData: withData,
		ConfigOnly:        len(entries) - withData,
	}, nil
}

// stageAllInstances dumps every instance into stagingDir/instances/<name>/,
// returning one manifest entry per instance.
func stageAllInstances(ctx context.Context, deps *Dependencies, stagingDir string) ([]SnapshotInstanceEntry, error) {
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
		}

		instanceDir := filepath.Join(stagingDir, snapshotInstancesDir, instance.Name)
		if err := os.MkdirAll(instanceDir, 0o750); err != nil {
			return nil, fmt.Errorf("create staging dir for %s: %w", instance.Name, err)
		}

		if instance.Status != "running" {
			// Configuration-only: instance.json needs no cluster connection.
			entry.SkipReason = fmt.Sprintf("instance was %s at snapshot time; databases not captured", instance.Status)
			log.Printf("WARNING: snapshot: instance %q is %s - capturing configuration only, NOT its databases",
				instance.Name, instance.Status)
			if err := writeInstanceMetadata(instanceDir, captureInstanceMetadata(deps, instance)); err != nil {
				return nil, fmt.Errorf("stage %s: %w", instance.Name, err)
			}
			entries = append(entries, entry)
			continue
		}

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
