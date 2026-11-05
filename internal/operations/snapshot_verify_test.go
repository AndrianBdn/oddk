package operations

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/rfc3339time"
	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
)

// snapshotFixture writes a minimal but structurally complete snapshot archive
// (manifest first, oddk.db holding dbContents, no instances) encrypted with c,
// and returns its path and SHA-256. Ordered as MakeSnapshot orders it; an
// empty dbContents leaves oddk.db out.
func snapshotFixture(t *testing.T, c *compression.Compressor, dbContents string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, snapshotManifestFile), []byte(`{"formatVersion":2,"instances":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := []compression.ArchiveEntry{
		{SourcePath: filepath.Join(src, snapshotManifestFile), ArchiveName: snapshotManifestFile},
	}
	if dbContents != "" {
		if err := os.WriteFile(filepath.Join(src, snapshotStoreFile), []byte(dbContents), 0o600); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, compression.ArchiveEntry{
			SourcePath: filepath.Join(src, snapshotStoreFile), ArchiveName: snapshotStoreFile,
		})
	}
	out := filepath.Join(dir, "snapshot"+compression.EncryptedSuffix)
	if _, err := c.CreateTarZstdOrdered(context.Background(), entries, out, nil); err != nil {
		t.Fatalf("build fixture archive: %v", err)
	}
	return out, fileSHA256(t, out)
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 - test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestVerifyLocalSnapshotCopy(t *testing.T) {
	ctx := context.Background()
	c := testCompressor(t)
	good, goodSHA := snapshotFixture(t, c, "db")
	other, _ := snapshotFixture(t, c, "a different db")
	incomplete, _ := snapshotFixture(t, c, "")

	corrupt := filepath.Join(t.TempDir(), "corrupt"+compression.EncryptedSuffix)
	b, err := os.ReadFile(good) // #nosec G304 - test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0x01
	if err := os.WriteFile(corrupt, b, 0o600); err != nil {
		t.Fatal(err)
	}

	foreignC, err := ArchiveCompressor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	foreign, foreignSHA := snapshotFixture(t, foreignC, "db")

	unreadable := filepath.Join(t.TempDir(), "unreadable"+compression.EncryptedSuffix)
	if err := os.WriteFile(unreadable, b, 0o000); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, path, want, status string
	}{
		{"matching digest", good, goodSHA, CopyVerified},
		{"no catalogued digest", good, "", CopyIntact},
		// A valid archive under the expected name: only the digest can tell.
		{"replaced by another valid archive", other, goodSHA, CopyMismatch},
		{"bit flip", corrupt, goodSHA, CopyDamaged},
		{"bit flip, no catalogued digest", corrupt, "", CopyDamaged},
		{"missing file", filepath.Join(t.TempDir(), "gone.tar.zst.age"), goodSHA, CopyMissing},
		// Without a digest the members are the only structural evidence.
		{"intact but not a complete snapshot", incomplete, "", CopyDamaged},
		// Not damage: the copy may be fine, it is this host's key that cannot open it.
		{"another host's key", foreign, foreignSHA, CopyUnchecked},
		// A file that cannot be opened says nothing about its bytes.
		{"permission denied", unreadable, goodSHA, CopyUnchecked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.path == unreadable && os.Geteuid() == 0 {
				t.Skip("root reads a 0000 file")
			}
			got := verifyLocalSnapshotCopy(ctx, c, tt.path, tt.want)
			if got.Status != tt.status {
				t.Fatalf("status = %q (%s), want %q", got.Status, got.Detail, tt.status)
			}
			if got.Location != tt.path {
				t.Errorf("location = %q, want %q", got.Location, tt.path)
			}
		})
	}
}

func TestCheckRemoteObject(t *testing.T) {
	ctx := context.Background()
	c := testCompressor(t)
	goodPath, goodSHA := snapshotFixture(t, c, "db")
	good, err := os.ReadFile(goodPath) // #nosec G304 - test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Clone(good)
	corrupt[len(corrupt)/2] ^= 0x01
	// Incompressible, so its archive cannot come out the same size as good's.
	otherPath, _ := snapshotFixture(t, c, rand.Text()+rand.Text())
	other, err := os.ReadFile(otherPath) // #nosec G304 - test-controlled path
	if err != nil {
		t.Fatal(err)
	}

	// Several age chunks (64 KiB each) with damage in the first: the verifier
	// stops while the download still has most of the body to write, which is
	// the path where the pipe must be closed to let the download end.
	noise := make([]byte, 4<<16)
	_, _ = rand.Read(noise)
	bigPath, bigSHA := snapshotFixture(t, c, string(noise))
	bigFlipped, err := os.ReadFile(bigPath) // #nosec G304 - test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if len(bigFlipped) < 3<<16 {
		t.Fatalf("fixture is %d bytes; it must span several age chunks", len(bigFlipped))
	}
	bigFlipped[1024] ^= 0x01

	// A valid archive that is not a snapshot (no oddk.db).
	notSnapshotPath, _ := snapshotFixture(t, c, "")
	notSnapshot, err := os.ReadFile(notSnapshotPath) // #nosec G304 - test-controlled path
	if err != nil {
		t.Fatal(err)
	}

	serve := func(body []byte, getStatus int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if body == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			if getStatus != http.StatusOK {
				w.Header().Del("Content-Length")
				w.WriteHeader(getStatus)
				return
			}
			_, _ = w.Write(body)
		}
	}

	tests := []struct {
		name      string
		handler   http.HandlerFunc
		wantSHA   string
		wantSize  int64
		deep      bool
		status    string
		detailHas string
	}{
		{"absent", serve(nil, 0), goodSHA, int64(len(good)), false, CopyMissing, ""},
		{"present, not read", serve(good, http.StatusOK), goodSHA, int64(len(good)), false, CopyPresent, "--deep"},
		{"wrong size", serve(other, http.StatusOK), goodSHA, int64(len(good)), false, CopyDamaged, "bytes"},
		{"deep, intact", serve(good, http.StatusOK), goodSHA, int64(len(good)), true, CopyVerified, ""},
		{"deep, no catalogued digest", serve(good, http.StatusOK), "", int64(len(good)), true, CopyIntact, ""},
		// With no digest, structure is the only evidence the stream can offer.
		{"deep, no digest, not a snapshot", serve(notSnapshot, http.StatusOK), "", int64(len(notSnapshot)), true, CopyDamaged, "not a snapshot"},
		{"deep, bit flip", serve(corrupt, http.StatusOK), goodSHA, int64(len(good)), true, CopyDamaged, ""},
		{"deep, damage early in a large object", serve(bigFlipped, http.StatusOK), bigSHA, int64(len(bigFlipped)), true, CopyDamaged, ""},
		// Same size so the HEAD passes; only the digest catches it.
		{"deep, replaced", serve(other, http.StatusOK), goodSHA, int64(len(other)), true, CopyMismatch, "NOT the archive"},
		// A refused GET says nothing about the object.
		{"deep, download refused", serve(good, http.StatusForbidden), goodSHA, int64(len(good)), true, CopyUnchecked, "download failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newFetchStubClient(t, tt.handler)
			got := checkRemoteObject(ctx, c, client, "snap/x.tar.zst.age",
				SnapshotCopyCheck{Location: "s3://b/snap/x.tar.zst.age"}, tt.wantSHA, tt.wantSize, tt.deep)
			if got.Status != tt.status {
				t.Fatalf("status = %q (%s), want %q", got.Status, got.Detail, tt.status)
			}
			if tt.detailHas != "" && !strings.Contains(got.Detail, tt.detailHas) {
				t.Errorf("detail %q does not mention %q", got.Detail, tt.detailHas)
			}
			if tt.status == CopyVerified && got.SHA256 != goodSHA {
				t.Errorf("sha256 = %q, want %q", got.SHA256, goodSHA)
			}
		})
	}
}

// The whole operation against a real catalogue: OK only when every listed
// copy is usable, and the offsite copy of a host with no offsite settings is
// reported as unchecked rather than silently passed.
func TestVerifySnapshot(t *testing.T) {
	ctx := context.Background()
	st, dir := newTestStore(t)
	c := testCompressor(t)
	deps := &Dependencies{Store: st, Archives: c}

	fixture, digest := snapshotFixture(t, c, "db")
	local := filepath.Join(dir, "snapshot-a"+compression.EncryptedSuffix)
	b, err := os.ReadFile(fixture) // #nosec G304 - test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, b, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &snapshotstore.Record{Filename: filepath.Base(local), LocalPath: local, Size: int64(len(b)), Status: "completed", SHA256Str: digest, CreatedAt: rfc3339time.Now()}
	if err := st.Snapshot.RecordSnapshot(rec); err != nil {
		t.Fatal(err)
	}

	res, err := VerifySnapshot(ctx, deps, rec.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Local == nil || res.Local.Status != CopyVerified || res.Remote != nil {
		t.Fatalf("intact local-only snapshot: %+v local=%+v remote=%+v", res, res.Local, res.Remote)
	}

	if err := st.Snapshot.SetRemoteLocation(rec.ID, "s3://bucket/key"); err != nil {
		t.Fatal(err)
	}
	res, err = VerifySnapshot(ctx, deps, rec.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Remote == nil || res.Remote.Status != CopyUnchecked {
		t.Fatalf("an S3 copy that cannot be reached must fail the check: %+v remote=%+v", res, res.Remote)
	}

	if _, err := VerifySnapshot(ctx, deps, rec.ID+100, false); err == nil {
		t.Fatal("verifying an unknown snapshot id should fail")
	}
}

// Retention runs concurrently with a verify, so a copy that vanished mid-check
// is only a problem while the catalogue still lists it.
func TestSettleRemovedCopies(t *testing.T) {
	before := &snapshotstore.Record{LocalPath: "/b/a", RemotePath: "s3://b/a"}
	problems := func() *VerifySnapshotResult {
		return &VerifySnapshotResult{
			Local:  &SnapshotCopyCheck{Status: CopyMissing},
			Remote: &SnapshotCopyCheck{Status: CopyDamaged, SHA256: "x"},
		}
	}

	tests := []struct {
		name          string
		now           *snapshotstore.Record
		local, remote string
	}{
		{"still catalogued", before, CopyMissing, CopyDamaged},
		{"local copy dropped", &snapshotstore.Record{RemotePath: "s3://b/a"}, CopyRemoved, CopyDamaged},
		{"remote copy dropped", &snapshotstore.Record{LocalPath: "/b/a"}, CopyMissing, CopyRemoved},
		{"row deleted", nil, CopyRemoved, CopyRemoved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := problems()
			settleRemovedCopies(r, before, tt.now)
			if r.Local.Status != tt.local || r.Remote.Status != tt.remote {
				t.Fatalf("local=%s remote=%s, want local=%s remote=%s", r.Local.Status, r.Remote.Status, tt.local, tt.remote)
			}
			if r.Remote.Status == CopyRemoved && r.Remote.SHA256 != "" {
				t.Error("a removed copy kept the digest of a read that no longer describes anything")
			}
		})
	}

	// A usable copy is never rewritten, and a copy the record did not list is skipped.
	r := &VerifySnapshotResult{Local: &SnapshotCopyCheck{Status: CopyVerified}}
	settleRemovedCopies(r, &snapshotstore.Record{LocalPath: "/b/a"}, nil)
	if r.Local.Status != CopyVerified || r.Remote != nil {
		t.Fatalf("settle touched a usable copy: local=%+v remote=%+v", r.Local, r.Remote)
	}
}
