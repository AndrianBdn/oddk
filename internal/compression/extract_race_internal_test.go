package compression

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildTestArchive writes a one-member archive whose manifest.json says body.
func buildTestArchive(t *testing.T, dir, name, body string) (path, digest string) {
	t.Helper()
	src := filepath.Join(dir, name+"-src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "manifest.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, name+".tar.zst")
	if _, err := NewCompressor().CreateTarZstd(context.Background(), src, path, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return path, hex.EncodeToString(sum[:])
}

func withAfterVerify(t *testing.T, fn func()) {
	t.Helper()
	afterVerifyHook = fn
	t.Cleanup(func() { afterVerifyHook = nil })
}

// A file renamed over the archive's path between verification and extraction
// must not be what gets extracted: verification and extraction read the same
// handle, so the verified archive is what comes out.
func TestExtract_RenameOverPathAfterVerifyExtractsTheVerifiedArchive(t *testing.T) {
	dir := t.TempDir()
	archive, digest := buildTestArchive(t, dir, "a", `{"archive":"A"}`)
	other, _ := buildTestArchive(t, dir, "b", `{"archive":"B, a different and perfectly valid one"}`)

	withAfterVerify(t, func() {
		if err := os.Rename(other, archive); err != nil {
			t.Fatal(err)
		}
	})
	dest := filepath.Join(dir, "out")
	if err := NewCompressor().ExtractTarZstdWith(context.Background(), archive, dest, ExtractOptions{WantSHA256: digest}); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"archive":"A"}` {
		t.Fatalf("extracted %s: the archive swapped in AFTER verification was restored", got)
	}
}

// The same file rewritten in place after verification cannot be pinned by a
// handle, so it must be detected and refused.
func TestExtract_InPlaceRewriteAfterVerifyIsRefused(t *testing.T) {
	dir := t.TempDir()
	archive, digest := buildTestArchive(t, dir, "a", `{"archive":"A"}`)
	other, _ := buildTestArchive(t, dir, "b", `{"archive":"B, a different and perfectly valid one"}`)
	replacement, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}

	withAfterVerify(t, func() {
		// os.WriteFile truncates and rewrites the SAME inode, as `cp` does.
		if err := os.WriteFile(archive, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	err = NewCompressor().ExtractTarZstdWith(context.Background(), archive, filepath.Join(dir, "out"),
		ExtractOptions{WantSHA256: digest})
	if !errors.Is(err, ErrArchiveChanged) {
		t.Fatalf("an archive rewritten in place after verification was not refused: %v", err)
	}
}

// Without a recorded digest (a --file restore) the same in-place rewrite is
// still refused: the stat check does not depend on having a digest.
func TestExtract_InPlaceRewriteRefusedWithoutDigest(t *testing.T) {
	dir := t.TempDir()
	archive, _ := buildTestArchive(t, dir, "a", `{"archive":"A"}`)
	other, _ := buildTestArchive(t, dir, "b", `{"archive":"B, a different and perfectly valid one"}`)
	replacement, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	withAfterVerify(t, func() {
		if err := os.WriteFile(archive, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	err = NewCompressor().ExtractTarZstd(context.Background(), archive, filepath.Join(dir, "out"))
	if !errors.Is(err, ErrArchiveChanged) {
		t.Fatalf("in-place rewrite not refused without a digest: %v", err)
	}
}

// buildRandomArchive writes a one-member archive of incompressible content, so
// two of them with different seeds are different archives of the SAME length
// (zstd stores incompressible blocks raw).
func buildRandomArchive(t *testing.T, dir, name string, seed int64) string {
	t.Helper()
	src := filepath.Join(dir, name+"-src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 64<<10)
	rand.New(rand.NewSource(seed)).Read(payload)
	if err := os.WriteFile(filepath.Join(src, "data.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	// Pin the member's mtime: it is in the tar header, and must not make the
	// two archives differ in anything but content.
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "data.bin"), fixed, fixed); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".tar.zst")
	if _, err := NewCompressor().CreateTarZstd(context.Background(), src, path, nil); err != nil {
		t.Fatal(err)
	}
	return path
}

// The case a size+mtime guard cannot see: a DIFFERENT valid archive of exactly
// the same length written over the file in place, with the original mtime put
// back (as `cp -p` / `rsync -t` do). Only comparing the bytes extracted with
// the bytes verified catches it.
func TestExtract_SameLengthRewriteWithRestoredMtimeIsRefused(t *testing.T) {
	dir := t.TempDir()
	archive := buildRandomArchive(t, dir, "a", 1)
	other := buildRandomArchive(t, dir, "b", 2)
	replacement, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != int64(len(replacement)) {
		t.Fatalf("fixture archives differ in length (%d vs %d); the test needs equal lengths", st.Size(), len(replacement))
	}

	withAfterVerify(t, func() {
		if err := os.WriteFile(archive, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(archive, st.ModTime(), st.ModTime()); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(archive)
		if err != nil || after.Size() != st.Size() || !after.ModTime().Equal(st.ModTime()) {
			t.Fatalf("fixture: size/mtime not preserved (%v, %v)", after, err)
		}
	})
	err = NewCompressor().ExtractTarZstd(context.Background(), archive, filepath.Join(dir, "out"))
	if !errors.Is(err, ErrArchiveChanged) {
		t.Fatalf("a same-length rewrite with its mtime restored was not refused: %v", err)
	}
}
