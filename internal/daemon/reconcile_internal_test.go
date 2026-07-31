package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/rfc3339time"
	"github.com/andrianbdn/oddk/internal/store"
	"github.com/andrianbdn/oddk/internal/store/backup"
)

func TestSweepBackupDir(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatal(err)
	}

	st, err := store.NewStore(filepath.Join(dataDir, "oddk.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}

	// Orphaned temp artifacts from interrupted operations — must be removed.
	staleDirs := []string{".tmp-backup-x-1", ".pgpass-123", ".restore-456", ".upgrade-789"}
	for _, dir := range staleDirs {
		if err := os.MkdirAll(filepath.Join(backupDir, dir, "sub"), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	// A recorded archive — must survive, record and file.
	referencedPath := filepath.Join(backupDir, "backup-app-20260101000000-1.tar.zst")
	if err := os.WriteFile(referencedPath, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	referenced := &backup.BackupRecord{
		InstanceName: "app",
		Timestamp:    rfc3339time.Now(),
		Size:         4,
		LocalPath:    referencedPath,
		Status:       "completed",
	}
	if err := st.Backup.RecordBackup(referenced); err != nil {
		t.Fatal(err)
	}

	// An archive no record references — must be kept (only reported).
	unreferencedPath := filepath.Join(backupDir, "manually-dropped.tar.zst")
	if err := os.WriteFile(unreferencedPath, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A record whose file is gone and that has no remote copy — the existing
	// ListAllBackups orphan cleanup must delete it during the sweep.
	orphan := &backup.BackupRecord{
		InstanceName: "app",
		Timestamp:    rfc3339time.Now(),
		Size:         4,
		LocalPath:    filepath.Join(backupDir, "backup-app-20260101000001-1.tar.zst"),
		Status:       "completed",
	}
	if err := st.Backup.RecordBackup(orphan); err != nil {
		t.Fatal(err)
	}

	sweepBackupDir(st, backupDir)

	for _, dir := range staleDirs {
		if _, err := os.Stat(filepath.Join(backupDir, dir)); !os.IsNotExist(err) {
			t.Errorf("stale artifact %s should have been removed", dir)
		}
	}
	if _, err := os.Stat(referencedPath); err != nil {
		t.Errorf("referenced archive should survive the sweep: %v", err)
	}
	if _, err := os.Stat(unreferencedPath); err != nil {
		t.Errorf("unreferenced archive should survive the sweep: %v", err)
	}

	records, err := st.Backup.ListAllBackups()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != referenced.ID {
		t.Errorf("expected only the referenced record to survive, got %d record(s)", len(records))
	}
}

// TestSweepBackupDirSnapshotArtifacts covers the two ways snapshots interact
// with the startup sweep, which differ from per-instance backups:
//
//   - An interrupted snapshot leaves a .snapshot-* staging directory holding
//     full database dumps. Left alone it would occupy that space forever, so it
//     must be swept like any other orphaned temp artifact.
//   - A finished snapshot archive is deliberately absent from backup_history
//     (it is a whole-deployment artifact, not a per-instance backup). It must
//     survive AND must not be reported as a stray archive, or every startup
//     would log a warning that trains operators to ignore the real ones.
func TestSweepBackupDirSnapshotArtifacts(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewStore(filepath.Join(dataDir, "oddk.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}

	stagingDir := filepath.Join(backupDir, ".snapshot-20260101000000")
	if err := os.MkdirAll(filepath.Join(stagingDir, "instances", "app", "databases"), 0o750); err != nil {
		t.Fatal(err)
	}

	snapshotArchive := filepath.Join(backupDir, "snapshot-db01-20260101000000.tar.zst")
	if err := os.WriteFile(snapshotArchive, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	sweepBackupDir(st, backupDir)

	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Error("interrupted snapshot staging directory should have been removed")
	}
	if _, err := os.Stat(snapshotArchive); err != nil {
		t.Errorf("finished snapshot archive should survive the sweep: %v", err)
	}
}
