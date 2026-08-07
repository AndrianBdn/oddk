package operations

import (
	"context"
	"database/sql"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/rfc3339time"
	backupstore "github.com/andrianbdn/oddk/internal/store/backup"
)

func dropAllFixture(t *testing.T) (*Dependencies, string) {
	t.Helper()
	st, dataDir := newTestStore(t)
	deps := &Dependencies{
		Store:   st,
		DataDir: dataDir,
		Logger:  log.New(io.Discard, "", 0),
	}
	return deps, dataDir
}

func writeArchive(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func recordFor(t *testing.T, deps *Dependencies, instance, localPath, remote string) int {
	t.Helper()
	rec := &backupstore.BackupRecord{
		InstanceName: instance,
		Timestamp:    rfc3339time.Now(),
		Size:         1,
		LocalPath:    localPath,
		Status:       "completed",
	}
	if remote != "" {
		rec.RemoteLocation = sql.NullString{String: remote, Valid: true}
	}
	if err := deps.Store.Backup.RecordBackup(rec); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

// The sweep must drop records for destroyed instances (no instance lookup),
// tolerate an already-missing local file, drop unreachable remote references
// when offsite is unconfigured, and KEEP any record whose local file could
// not be removed — so a partial failure is re-runnable, never silent loss of
// the catalogue's honesty.
func TestDropAllBackups(t *testing.T) {
	deps, dataDir := dropAllFixture(t)
	backupDir := filepath.Join(dataDir, "backups")
	if err := os.Mkdir(backupDir, 0o750); err != nil {
		t.Fatal(err)
	}

	fileA := writeArchive(t, backupDir, "a.tar.zst", 100)
	recordFor(t, deps, "gone-instance", fileA, "")
	recordFor(t, deps, "app", filepath.Join(backupDir, "missing.tar.zst"), "")
	fileC := writeArchive(t, backupDir, "c.tar.zst", 50)
	recordFor(t, deps, "app", fileC, "s3://bucket/backups/c.tar.zst")
	recordFor(t, deps, "app", "", "s3://bucket/backups/remote-only.tar.zst")

	// A file whose parent directory is read-only cannot be unlinked.
	lockedDir := filepath.Join(backupDir, "locked")
	if err := os.Mkdir(lockedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	fileE := writeArchive(t, lockedDir, "e.tar.zst", 10)
	if err := os.Chmod(lockedDir, 0o550); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o750) })
	keptID := recordFor(t, deps, "app", fileE, "")

	result, err := DropAllBackups(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}

	if result.RecordsTotal != 5 || result.RecordsDropped != 4 || result.RecordsKept != 1 {
		t.Fatalf("unexpected counts: %+v", result)
	}
	if result.LocalFilesDeleted != 2 || result.LocalFilesMissing != 1 {
		t.Fatalf("unexpected local counts: %+v", result)
	}
	if result.LocalBytesFreed != 150 {
		t.Fatalf("bytes freed = %d, want 150", result.LocalBytesFreed)
	}
	if result.RemoteRefsDroppedNoOffsite != 2 || result.RemoteObjectsDeleted != 0 {
		t.Fatalf("unexpected remote counts: %+v", result)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one", result.Errors)
	}
	for _, path := range []string{fileA, fileC} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s should be deleted", path)
		}
	}
	if _, err := os.Stat(fileE); err != nil {
		t.Fatalf("undeletable file must survive: %v", err)
	}

	remaining, err := deps.Store.Backup.AllRecordsRaw()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != keptID {
		t.Fatalf("remaining records = %+v, want only the kept one", remaining)
	}

	// Fix the cause and re-run: the kept record drains, and a third run over an
	// empty catalogue is a quiet success.
	if err := os.Chmod(lockedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	result, err = DropAllBackups(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecordsTotal != 1 || result.RecordsDropped != 1 || len(result.Errors) != 0 {
		t.Fatalf("re-run should drain the kept record: %+v", result)
	}

	result, err = DropAllBackups(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecordsTotal != 0 || result.Message != "No backup records found — nothing to do" {
		t.Fatalf("empty catalogue should be a quiet no-op: %+v", result)
	}
}
