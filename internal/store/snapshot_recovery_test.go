package store_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/rfc3339time"
	"github.com/andrianbdn/oddk/internal/store"
	"github.com/andrianbdn/oddk/internal/store/backup"
)

// TestReconcileLocalLocations covers the three outcomes after a snapshot apply,
// where every backup_history row carries the SOURCE host's paths.
//
// Without this reconciliation the first ListAllBackups — run by daemon startup
// and again by 'oddk checklist', the two things apply tells the operator to do
// next — hard-deletes every local-only record whose file is missing, silently
// destroying the backup catalogue of a deployment with no offsite.
func TestReconcileLocalLocations(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewStore(filepath.Join(dataDir, "oddk.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}

	// Present here: the operator copied the backup directory across, so the
	// record must be re-pointed rather than dropped.
	presentName := "backup-app-20260101000000-1.tar.zst"
	if err := os.WriteFile(filepath.Join(backupDir, presentName), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	present := &backup.BackupRecord{
		InstanceName: "app", Timestamp: rfc3339time.Now(), Size: 4, Status: "completed",
		LocalPath: "/srv/oldhost/backups/" + presentName,
	}
	if err := st.Backup.RecordBackup(present); err != nil {
		t.Fatal(err)
	}

	// Absent here, but has an offsite copy: local cleared, record survives.
	withRemote := &backup.BackupRecord{
		InstanceName: "app", Timestamp: rfc3339time.Now(), Size: 4, Status: "completed",
		LocalPath:      "/srv/oldhost/backups/backup-app-20260101000001-1.tar.zst",
		RemoteLocation: sql.NullString{String: "s3://bucket/backup-app-20260101000001-1.tar.zst", Valid: true},
	}
	if err := st.Backup.RecordBackup(withRemote); err != nil {
		t.Fatal(err)
	}

	// Absent here and no offsite: nothing can recover it, so it must be
	// COUNTED and reported rather than silently dropped later.
	danglingRec := &backup.BackupRecord{
		InstanceName: "app", Timestamp: rfc3339time.Now(), Size: 4, Status: "completed",
		LocalPath: "/srv/oldhost/backups/backup-app-20260101000002-1.tar.zst",
	}
	if err := st.Backup.RecordBackup(danglingRec); err != nil {
		t.Fatal(err)
	}

	repointed, cleared, dangling, err := st.Backup.ReconcileLocalLocations(backupDir)
	if err != nil {
		t.Fatalf("ReconcileLocalLocations: %v", err)
	}
	if repointed != 1 {
		t.Errorf("repointed = %d, want 1", repointed)
	}
	// Only the record with an offsite copy can have its stale local path
	// cleared; the schema's CHECK forbids a row with neither location.
	if cleared != 1 {
		t.Errorf("cleared = %d, want 1", cleared)
	}
	if dangling != 1 {
		t.Errorf("dangling = %d, want 1", dangling)
	}

	// The decisive assertion: the re-pointed record must now survive the very
	// sweep that would previously have deleted it.
	records, err := st.Backup.ListAllBackups()
	if err != nil {
		t.Fatal(err)
	}
	var foundPresent, foundRemote bool
	for _, r := range records {
		switch r.ID {
		case present.ID:
			foundPresent = true
			if r.LocalPath != filepath.Join(backupDir, presentName) {
				t.Errorf("re-pointed local path = %q, want %q", r.LocalPath, filepath.Join(backupDir, presentName))
			}
		case withRemote.ID:
			foundRemote = true
		}
	}
	if !foundPresent {
		t.Error("record whose file IS present on this host was dropped by the sweep")
	}
	if !foundRemote {
		t.Error("record with an offsite copy was dropped by the sweep")
	}
}

// TestDataDirLockIsExclusive pins the guard that replaced the point-in-time
// port probe: while apply holds the lock, a daemon start must fail rather than
// race it.
func TestDataDirLockIsExclusive(t *testing.T) {
	dataDir := t.TempDir()

	first, err := store.AcquireDataDirLock(dataDir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if second, err := store.AcquireDataDirLock(dataDir); err == nil {
		second.Release()
		t.Fatal("second acquire succeeded while the lock was held")
	}

	first.Release()

	// Released locks must be re-acquirable, or a daemon could never restart
	// after an apply.
	third, err := store.AcquireDataDirLock(dataDir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	third.Release()

	// Release must be idempotent - Shutdown defers it and the process exit
	// would otherwise double-close.
	third.Release()
}

// TestDataDirLockSeparateDirs guards against a global lock: two deployments on
// one host must not block each other.
func TestDataDirLockSeparateDirs(t *testing.T) {
	a, err := store.AcquireDataDirLock(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()

	b, err := store.AcquireDataDirLock(t.TempDir())
	if err != nil {
		t.Fatalf("lock on a different data dir was blocked: %v", err)
	}
	b.Release()
}
