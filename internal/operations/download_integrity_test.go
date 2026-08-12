package operations

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The catalogued-download path (`backup download`, `snapshot download <id>`)
// must not hand a corrupt archive a real archive name and a catalogue row that
// claims a local copy. Before this, S3 integrity was ContentLength plus an ETag
// sidecar — provenance, not integrity — so damage was discovered at restore
// time, which is the worst possible moment to discover it.
func TestStreamToLocalFileAtomic_RefusesCorruptObject(t *testing.T) {
	good := tinyArchiveBytes(t, "intact")
	corrupt := make([]byte, len(good))
	copy(corrupt, good)
	corrupt[len(corrupt)/2] ^= 0x01

	client := newFetchStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(corrupt)))
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			_, _ = w.Write(corrupt)
		}
	})

	dir := t.TempDir()
	dest := filepath.Join(dir, "backup-app-20260811-7.tar.zst")

	if _, err := streamToLocalFileAtomic(context.Background(), client, "k/backup.tar.zst", dest); err == nil {
		t.Fatal("a corrupt object was accepted; the catalogue would now claim a local copy that cannot be restored")
	}

	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("a corrupt download landed at the final archive name")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("%s was left behind by a refused download", e.Name())
	}
}

func TestStreamToLocalFileAtomic_LandsAGoodArchive(t *testing.T) {
	good := tinyArchiveBytes(t, "intact")

	client := newFetchStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(good)))
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			_, _ = w.Write(good)
		}
	})

	dir := t.TempDir()
	dest := filepath.Join(dir, "snapshot-db01-20260811090000.tar.zst")

	written, err := streamToLocalFileAtomic(context.Background(), client, "k/snap.tar.zst", dest)
	if err != nil {
		t.Fatalf("a good archive was refused: %v", err)
	}
	if written != int64(len(good)) {
		t.Errorf("wrote %d bytes, want %d", written, len(good))
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(good) {
		t.Fatalf("downloaded content = %d bytes/%v", len(got), err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file %s left behind after a successful download", e.Name())
		}
	}

	// A downloaded archive is as sensitive as a written one: database contents
	// plus role password hashes, unencrypted. writeVerifiedArchive already
	// enforces 0600 on the write path; os.Create would have landed 0644 here.
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat downloaded archive: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("downloaded archive mode = %v, want 0600 — it holds database contents and role password hashes", fi.Mode().Perm())
	}
}

// copyFile installs the snapshot's oddk.db onto a disaster-recovery host, which
// by definition holds no other copy of that state. It must publish the file only
// once it is complete and on disk.
func TestCopyFile_PublishesAtomically(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "oddk.db.src")
	body := strings.Repeat("sqlite-bytes", 1000)
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "oddk.db")
	if err := copyFile(src, dst, 0o600); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil || string(got) != body {
		t.Fatalf("copied content = %d bytes/%v, want %d bytes", len(got), err, len(body))
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 — oddk.db holds encrypted credentials", fi.Mode().Perm())
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}
