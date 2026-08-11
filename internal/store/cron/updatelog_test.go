package cron_test

import (
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"github.com/andrianbdn/oddk/internal/store/cron"
)

// newTestCronStore builds an in-memory cron_logs table matching the migration.
func newTestCronStore(t *testing.T) (*cron.CronStore, *sqlx.DB) {
	t.Helper()

	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	db.MustExec(`CREATE TABLE cron_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		instance_name TEXT NOT NULL,
		started_at TEXT NOT NULL,
		completed_at TEXT,
		backup_status TEXT,
		backup_finished_at TEXT,
		backup_error TEXT,
		backup_upload_status TEXT,
		backup_upload_finished_at TEXT,
		backup_upload_error TEXT,
		backup_cleanup_status TEXT,
		backup_cleanup_finished_at TEXT,
		backup_cleanup_error TEXT,
		backup_remote_cleanup_status TEXT,
		backup_remote_cleanup_finished_at TEXT,
		backup_remote_cleanup_error TEXT
	)`)

	return cron.NewCronStore(db), db
}

// The regression: UpdateLog takes map[string]any, so callers handed the driver a
// bare time.Time. That bypasses rfc3339time's driver.Valuer and stores Go's
// String() layout into a TEXT column the scanner expects to be RFC3339 — the row
// wrote fine and then could never be read back. It went unnoticed for the life
// of the feature because nothing read cron_logs at all.
func TestUpdateLog_NormalizesBareTimeTime(t *testing.T) {
	store, db := newTestCronStore(t)

	created, err := store.CreateLog("billing")
	if err != nil {
		t.Fatalf("CreateLog: %v", err)
	}

	finishedAt := time.Date(2026, 8, 11, 4, 53, 54, 122388675, time.UTC)
	err = store.UpdateLog(created.ID, map[string]any{
		"backup_status":      "ok",
		"backup_finished_at": finishedAt, // a BARE time.Time, as the ops layer passes
	})
	if err != nil {
		t.Fatalf("UpdateLog: %v", err)
	}

	// The actual failure mode was on read, not write.
	logs, err := store.ListAllLogs(10)
	if err != nil {
		t.Fatalf("ListAllLogs after writing a bare time.Time: %v\n"+
			"This is the bug: the row was written in a format the scanner cannot parse.", err)
	}
	if len(logs) != 1 {
		t.Fatalf("got %d logs, want 1", len(logs))
	}

	got := logs[0]
	if got.BackupFinishedAt == nil {
		t.Fatal("backup_finished_at came back nil")
	}
	if !got.BackupFinishedAt.Equal(finishedAt) {
		t.Errorf("backup_finished_at = %v, want %v", got.BackupFinishedAt.Time, finishedAt)
	}

	// Verify what actually landed in the column, so a future change that stores
	// a parseable-but-non-canonical format is still caught.
	var stored string
	if err := db.Get(&stored, `SELECT backup_finished_at FROM cron_logs WHERE id = ?`, created.ID); err != nil {
		t.Fatalf("read raw column: %v", err)
	}
	if stored != "2026-08-11T04:53:54.122388675Z" {
		t.Errorf("stored %q, want canonical RFC3339 with fixed-width nanoseconds.\n"+
			"Fixed width matters: retention compares these TEXT columns directly.", stored)
	}
}

// ListAllLogs is what makes the whole history readable; ListLogs needs an
// instance name and so cannot see the whole-deployment snapshot runs alongside
// per-instance ones.
func TestListAllLogs_SpansInstancesNewestFirst(t *testing.T) {
	store, _ := newTestCronStore(t)

	for _, name := range []string{"billing", "*snapshot*", "reporting"} {
		if _, err := store.CreateLog(name); err != nil {
			t.Fatalf("CreateLog(%s): %v", name, err)
		}
		time.Sleep(2 * time.Millisecond) // distinct started_at
	}

	logs, err := store.ListAllLogs(0)
	if err != nil {
		t.Fatalf("ListAllLogs: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("got %d logs, want 3 across all instances", len(logs))
	}
	if logs[0].InstanceName != "reporting" {
		t.Errorf("newest-first ordering broken: first row is %q, want %q", logs[0].InstanceName, "reporting")
	}

	limited, err := store.ListAllLogs(2)
	if err != nil {
		t.Fatalf("ListAllLogs(2): %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limit not applied: got %d rows, want 2", len(limited))
	}
}
