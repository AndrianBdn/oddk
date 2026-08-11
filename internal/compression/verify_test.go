package compression_test

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/compression"
)

// These tests exist because the restore path could not detect corruption at all.
// Measured against this package's own writer, 40 random single-bit flips in a
// 4 MB archive: ExtractTarZstd missed 40/40 and silently extracted DIFFERENT
// data with err == nil, every time. A tar reader stops at the tar zero-block
// terminator, which sits before the end of the zstd frame, so the frame's
// content checksum — the only thing that can see a flipped bit — was never read.
//
// Draining the decompressor to EOF is the fix, and it is the whole integrity
// mechanism. If a future refactor drops the drain, these tests are what catches
// it, so do not "simplify" them into a happy-path round trip.

func buildArchive(t *testing.T, dir string) string {
	t.Helper()

	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(src, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", []byte(`{"formatVersion":2}`))
	write("globals.sql", []byte(strings.Repeat("CREATE ROLE app;\n", 20000)))
	// Incompressible payload, so a flipped bit lands in real content rather than
	// in a run that zstd would have collapsed anyway.
	payload := make([]byte, 2<<20)
	rand.New(rand.NewSource(7)).Read(payload)
	write("data.bin", payload)

	archive := filepath.Join(dir, "a.tar.zst")
	if _, err := compression.NewCompressor().CreateTarZstd(context.Background(), src, archive, nil); err != nil {
		t.Fatalf("create archive: %v", err)
	}
	return archive
}

func mutate(t *testing.T, archive, name string, fn func([]byte) []byte) string {
	t.Helper()
	b, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(filepath.Dir(archive), name)
	if err := os.WriteFile(out, fn(b), 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVerifyTarZstd_AcceptsAGoodArchive(t *testing.T) {
	archive := buildArchive(t, t.TempDir())

	members, err := compression.NewCompressor().VerifyTarZstd(context.Background(), archive)
	if err != nil {
		t.Fatalf("a freshly written archive failed verification: %v", err)
	}

	found := map[string]bool{}
	for _, m := range members {
		found[m.Name] = true
	}
	for _, want := range []string{"manifest.json", "globals.sql", "data.bin"} {
		if !found[want] {
			t.Errorf("member %q missing from %v", want, found)
		}
	}
}

// The core guard. A single flipped bit anywhere in the compressed stream must be
// caught — this is what the old extract path missed 40 times out of 40.
func TestVerifyTarZstd_CatchesSingleBitFlips(t *testing.T) {
	dir := t.TempDir()
	archive := buildArchive(t, dir)
	orig, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}

	const trials = 25
	r := rand.New(rand.NewSource(99))
	missed := 0

	for i := range trials {
		lo, hi := len(orig)/10, len(orig)-len(orig)/10
		pos := lo + r.Intn(hi-lo)
		bit := uint(r.Intn(8))

		bad := mutate(t, archive, "bad.tar.zst", func(b []byte) []byte {
			c := make([]byte, len(b))
			copy(c, b)
			c[pos] ^= 1 << bit
			return c
		})

		if _, err := compression.NewCompressor().VerifyTarZstd(context.Background(), bad); err == nil {
			missed++
			t.Errorf("trial %d: flip at byte %d bit %d went undetected", i, pos, bit)
		}
	}
	if missed > 0 {
		t.Fatalf("%d/%d single-bit corruptions went undetected — the zstd drain is not running", missed, trials)
	}
}

// Every restore in ODDK goes through ExtractTarZstd. It must refuse a corrupt
// archive rather than write a silently different tree to disk.
func TestExtractTarZstd_RefusesCorruptArchiveWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	archive := buildArchive(t, dir)

	bad := mutate(t, archive, "bad.tar.zst", func(b []byte) []byte {
		c := make([]byte, len(b))
		copy(c, b)
		c[len(c)/2] ^= 0x01
		return c
	})

	dest := filepath.Join(dir, "dest")
	err := compression.NewCompressor().ExtractTarZstd(context.Background(), bad, dest)
	if err == nil {
		t.Fatal("ExtractTarZstd accepted a corrupt archive — a restore would have " +
			"written silently wrong data and reported success")
	}

	// Verification runs before anything is created, so the destination must not
	// even exist: a half-written tree invites someone to use it.
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("destination %s was created despite verification failing", dest)
	}
}

func TestVerifyTarZstd_CatchesTruncation(t *testing.T) {
	dir := t.TempDir()
	archive := buildArchive(t, dir)

	cases := map[string]func([]byte) []byte{
		"half":            func(b []byte) []byte { return b[:len(b)/2] },
		"last 4 bytes":    func(b []byte) []byte { return b[:len(b)-4] },
		"last byte":       func(b []byte) []byte { return b[:len(b)-1] },
		"trailing junk":   func(b []byte) []byte { return append(append([]byte{}, b...), 0xde, 0xad, 0xbe, 0xef) },
		"leading garbage": func(b []byte) []byte { return append([]byte{0x00, 0x01}, b...) },
	}

	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			bad := mutate(t, archive, "bad.tar.zst", fn)
			if _, err := compression.NewCompressor().VerifyTarZstd(context.Background(), bad); err == nil {
				t.Errorf("%s went undetected", name)
			}
		})
	}
}

// The one hole the stream check cannot see, pinned so nobody assumes otherwise:
// an empty file is a VALID empty zstd-less stream with zero members. Rejecting it
// is the caller's job, via the member assertion — which is exactly why the
// writers take a verify callback.
func TestVerifyTarZstd_ZeroLengthFileYieldsNoMembers(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.tar.zst")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	members, err := compression.NewCompressor().VerifyTarZstd(context.Background(), empty)
	if err == nil && len(members) != 0 {
		t.Fatalf("expected zero members, got %d", len(members))
	}
	if len(members) != 0 {
		t.Fatalf("a zero-length file reported %d members", len(members))
	}
	// Whether the stream check errors here is an implementation detail of the
	// zstd reader; what MUST hold is that no members are reported, so a caller's
	// member assertion rejects it.
}

// A rejected archive must leave nothing at the final path. The daemon's startup
// sweep only REPORTS unreferenced archives and never deletes them, so a retained
// corrupt file would be permanent litter in the directory an operator greps
// during a disaster — indistinguishable by name from a real archive.
func TestCreateTarZstd_RejectedArchiveIsNotPublished(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(dir, "out.tar.zst")
	_, err := compression.NewCompressor().CreateTarZstd(context.Background(), src, archive,
		func([]compression.Member) error { return os.ErrInvalid })
	if err == nil {
		t.Fatal("writer ignored the verify callback's rejection")
	}

	if _, statErr := os.Stat(archive); !os.IsNotExist(statErr) {
		t.Error("a rejected archive was published to the final path")
	}

	// And no temp litter either.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file %s left behind after a rejected write", e.Name())
		}
	}
}

// The archive must appear at its final path only once it is complete: a reader
// that sees the name must be able to trust the bytes.
func TestCreateTarZstd_PublishesAtomicallyAndVerifiably(t *testing.T) {
	dir := t.TempDir()
	archive := buildArchive(t, dir)

	fi, err := os.Stat(archive)
	if err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("archive mode is %v, want 0600 — archives hold database contents and role password hashes", fi.Mode().Perm())
	}
	if _, err := compression.NewCompressor().VerifyTarZstd(context.Background(), archive); err != nil {
		t.Errorf("published archive does not verify: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file %s left behind after a successful write", e.Name())
		}
	}
}
