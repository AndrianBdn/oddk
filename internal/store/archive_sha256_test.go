package store_test

import (
	"testing"

	"github.com/andrianbdn/oddk/internal/rfc3339time"
	"github.com/andrianbdn/oddk/internal/store/backup"
	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
)

const (
	digestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// Migration 022: the digest recorded at write time round-trips, and is the
// REFERENCE — a later download can fill in an unknown one, but must never
// replace a known one, or a divergent copy would become the new truth.
func TestSnapshotSHA256_RecordedAndNeverOverwritten(t *testing.T) {
	st := newTestStore(t)

	rec := &snapshotstore.Record{
		Filename: "snapshot-a.tar.zst", CreatedAt: rfc3339time.Now(), Size: 1,
		Status: "completed", Format: "physical", LocalPath: "/b/snapshot-a.tar.zst",
		SHA256Str: digestA,
	}
	if err := st.Snapshot.RecordSnapshot(rec); err != nil {
		t.Fatal(err)
	}
	if err := st.Snapshot.SetSHA256IfUnknown(rec.ID, digestB); err != nil {
		t.Fatal(err)
	}
	got, err := st.Snapshot.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256Str != digestA {
		t.Errorf("recorded digest = %q, want the write-time %q", got.SHA256Str, digestA)
	}

	// A pre-022 row reads back unknown, and learns its digest once.
	legacy := &snapshotstore.Record{
		Filename: "snapshot-b.tar.zst", CreatedAt: rfc3339time.Now(), Size: 1,
		Status: "completed", Format: "logical", LocalPath: "/b/snapshot-b.tar.zst",
	}
	if err := st.Snapshot.RecordSnapshot(legacy); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Snapshot.Get(legacy.ID)
	if got.SHA256.Valid {
		t.Fatalf("a row written with no digest reads back %q, want NULL (unknown)", got.SHA256.String)
	}
	if err := st.Snapshot.SetSHA256IfUnknown(legacy.ID, digestB); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Snapshot.Get(legacy.ID)
	if got.SHA256Str != digestB {
		t.Errorf("unknown digest was not filled in: %q", got.SHA256Str)
	}
}

func TestBackupSHA256_RecordedAndNeverOverwritten(t *testing.T) {
	st := newTestStore(t)

	rec := &backup.BackupRecord{
		InstanceName: "app", Timestamp: rfc3339time.Now(), Size: 1,
		LocalPath: "/b/backup-app-1.tar.zst", Status: "completed", SHA256Str: digestA,
	}
	if err := st.Backup.RecordBackup(rec); err != nil {
		t.Fatal(err)
	}
	if err := st.Backup.SetSHA256IfUnknown(rec.ID, digestB); err != nil {
		t.Fatal(err)
	}
	got, err := st.Backup.GetBackupByID(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256Str != digestA {
		t.Errorf("recorded digest = %q, want the write-time %q", got.SHA256Str, digestA)
	}
}
