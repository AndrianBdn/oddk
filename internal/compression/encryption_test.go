package compression_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/compression"
)

// What an encrypted archive must look like on disk: age, not zstd, and nothing
// of the content readable without the key.
func TestCreate_WritesAnAgeFileWithNoPlaintext(t *testing.T) {
	dir := t.TempDir()
	archive := buildArchive(t, dir)
	b, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"manifest.json", "globals.sql", "CREATE ROLE", "formatVersion"} {
		if bytes.Contains(b, []byte(leak)) {
			t.Errorf("archive contains %q in the clear", leak)
		}
	}
	if enc, err := compression.IsEncrypted(archive); err != nil || !enc {
		t.Errorf("IsEncrypted = %v, %v", enc, err)
	}
}

// There is no way to write a plaintext archive: no key, no archive.
func TestCreate_RefusesWithoutAKey(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "a"+compression.EncryptedSuffix)
	if _, err := compression.NewCompressor().CreateTarZstd(context.Background(), src, archive, nil); err == nil {
		t.Fatal("an archive was written without a key")
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Error("something was published")
	}
}

// The name must say what the bytes are.
func TestCreate_RefusesThePlainSuffix(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newC(t).CreateTarZstd(context.Background(), src, filepath.Join(dir, "a.tar.zst"), nil); err == nil {
		t.Fatal("an encrypted archive was written under a .tar.zst name")
	}
}

// A wrong key is refused as such, before anything is created, and the error
// names both keys so the operator knows which master.key to fetch.
func TestWrongKey_RefusedWithBothKeyIDs(t *testing.T) {
	dir := t.TempDir()
	archive := buildArchive(t, dir) // key 1
	wrong := compression.NewCompressor(compression.KeyForTest(t, 2))

	dest := filepath.Join(dir, "out")
	err := wrong.ExtractTarZstd(context.Background(), archive, dest)
	if !errors.Is(err, compression.ErrNotDecryptable) {
		t.Fatalf("got %v, want ErrNotDecryptable", err)
	}
	for _, id := range []string{compression.KeyForTest(t, 1).ID(), compression.KeyForTest(t, 2).ID()} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error does not name key %s: %v", id, err)
		}
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Error("a refused extraction created its destination")
	}

	for name, fn := range map[string]func() error{
		"verify":      func() error { _, err := wrong.VerifyTarZstd(context.Background(), archive); return err },
		"digest":      func() error { _, err := wrong.VerifyTarZstdDigest(context.Background(), archive); return err },
		"read member": func() error { _, err := wrong.ReadMember(archive, "manifest.json", 1<<20); return err },
		"no key": func() error {
			_, err := compression.NewCompressor().VerifyTarZstd(context.Background(), archive)
			return err
		},
	} {
		if err := fn(); !errors.Is(err, compression.ErrNotDecryptable) {
			t.Errorf("%s: got %v, want ErrNotDecryptable", name, err)
		}
	}
}

// Any one of the supplied keys opens the archive: a restore passes both this
// host's key and the foreign archive's.
func TestAnySuppliedKeyDecrypts(t *testing.T) {
	archive := buildArchive(t, t.TempDir()) // key 1
	c := compression.NewCompressor(compression.KeyForTest(t, 2), compression.KeyForTest(t, 1))
	if _, err := c.VerifyTarZstd(context.Background(), archive); err != nil {
		t.Fatalf("second key not tried: %v", err)
	}
}

// A plaintext archive from ODDK <= 0.1.93 stays readable, with or without a key.
func TestPlainArchivesStayReadable(t *testing.T) {
	dir := t.TempDir()
	archive := buildArchiveOf(t, dir, archiveKinds[1])
	for name, c := range map[string]*compression.Compressor{"keyed": newC(t), "keyless": compression.NewCompressor()} {
		if _, err := c.VerifyTarZstd(context.Background(), archive); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if err := c.ExtractTarZstd(context.Background(), archive, filepath.Join(dir, name)); err != nil {
			t.Errorf("%s extract: %v", name, err)
		}
	}
	if enc, err := compression.IsEncrypted(archive); err != nil || enc {
		t.Errorf("IsEncrypted(plain) = %v, %v", enc, err)
	}
}

// Damage to the age header, and a cut at every chunk boundary, are refused.
// Chunks are 64 KiB of plaintext plus a 16-byte tag; a cut exactly between two
// chunks is the case a reader that stops early would miss.
func TestEncrypted_HeaderDamageAndChunkBoundaryCuts(t *testing.T) {
	dir := t.TempDir()
	archive := buildArchive(t, dir)
	orig, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	headerEnd := bytes.Index(orig, []byte("\n--- ")) + 1
	headerEnd += bytes.IndexByte(orig[headerEnd:], '\n') + 1
	payloadStart := headerEnd + 16 // the payload opens with a 16-byte nonce

	cases := map[string]func([]byte) []byte{
		"flip in recipient stanza": func(b []byte) []byte { b[40] ^= 1; return b },
		"flip in recipient body":   func(b []byte) []byte { b[headerEnd/2] ^= 1; return b },
		"flip in header MAC":       func(b []byte) []byte { b[headerEnd-3] ^= 1; return b },
		"flip in payload nonce":    func(b []byte) []byte { b[headerEnd+3] ^= 1; return b },
		"flip in last chunk":       func(b []byte) []byte { b[len(b)-20] ^= 1; return b },
		"cut after header":         func(b []byte) []byte { return b[:headerEnd] },
		// A payload with no chunks at all: age reports it as io.ErrUnexpectedEOF,
		// which zstd took for an empty stream until payloadReader.
		"cut after nonce": func(b []byte) []byte { return b[:payloadStart] },
	}
	const chunk = 64<<10 + 16
	for off := payloadStart + chunk; off < len(orig); off += chunk {
		cases["cut at chunk boundary "+strconv.Itoa(off)] = func(b []byte) []byte { return b[:off] }
	}
	if len(cases) < 8 {
		t.Fatalf("archive too small to cross chunk boundaries (%d bytes)", len(orig))
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			bad := mutate(t, archive, "bad"+compression.EncryptedSuffix, func(b []byte) []byte {
				return fn(append([]byte{}, b...))
			})
			if _, err := newC(t).VerifyTarZstd(context.Background(), bad); err == nil {
				t.Errorf("%s went undetected", name)
			}
			if err := newC(t).ExtractTarZstd(context.Background(), bad, filepath.Join(t.TempDir(), "out")); err == nil {
				t.Errorf("%s was extracted", name)
			}
		})
	}
}

func TestReadMember(t *testing.T) {
	forEachKind(t, func(t *testing.T, kind archiveKind) {
		archive := buildArchiveOf(t, t.TempDir(), kind)
		got, err := newC(t).ReadMember(archive, "manifest.json", 1<<20)
		if err != nil || string(got) != `{"formatVersion":2}` {
			t.Fatalf("ReadMember = %q, %v", got, err)
		}
		if _, err := newC(t).ReadMember(archive, "nope.json", 1<<20); !errors.Is(err, compression.ErrMemberNotFound) {
			t.Fatalf("missing member: got %v", err)
		}
	})
}

func TestDecryptTo(t *testing.T) {
	dir := t.TempDir()
	archive := buildArchive(t, dir)
	out := filepath.Join(dir, "plain.tar.zst")
	if err := newC(t).DecryptTo(archive, out); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("output mode %v, want 0600", fi.Mode().Perm())
	}
	// The output is a plain archive that verifies and holds the same members.
	if enc, _ := compression.IsEncrypted(out); enc {
		t.Fatal("output is still encrypted")
	}
	members, err := compression.NewCompressor().VerifyTarZstd(context.Background(), out)
	if err != nil || len(members) != 3 {
		t.Fatalf("decrypted archive: %d members, %v", len(members), err)
	}

	if err := newC(t).DecryptTo(archive, out); err == nil {
		t.Error("an existing output was overwritten")
	}
	if err := newC(t).DecryptTo(out, filepath.Join(dir, "again.tar.zst")); err == nil {
		t.Error("a plain archive was 'decrypted'")
	}
	err = compression.NewCompressor(compression.KeyForTest(t, 2)).DecryptTo(archive, filepath.Join(dir, "wrong.tar.zst"))
	if !errors.Is(err, compression.ErrNotDecryptable) {
		t.Errorf("wrong key: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "wrong.tar.zst")); !os.IsNotExist(statErr) {
		t.Error("a refused decrypt left output behind")
	}
}

// The catalogue digest of an encrypted archive is the sha256sum of the
// ciphertext file, and a mismatch is still ErrDigestMismatch.
func TestEncrypted_DigestIsOfTheCiphertext(t *testing.T) {
	archive := buildArchive(t, t.TempDir())
	b, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	v, err := newC(t).VerifyTarZstdDigest(context.Background(), archive)
	if err != nil || v.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest %v, %v", v, err)
	}
	err = newC(t).ExtractTarZstdWith(context.Background(), archive, filepath.Join(t.TempDir(), "out"),
		compression.ExtractOptions{WantSHA256: strings.Repeat("0", 64)})
	if !errors.Is(err, compression.ErrDigestMismatch) {
		t.Fatalf("got %v, want ErrDigestMismatch", err)
	}
}

// Damage to the recipient stanza must read as corruption, not as a wrong key:
// the header still names the key that was supplied.
func TestEncrypted_DamagedRecipientIsCorruptNotWrongKey(t *testing.T) {
	archive := buildArchive(t, t.TempDir())
	bad := mutate(t, archive, "bad"+compression.EncryptedSuffix, func(b []byte) []byte {
		headerEnd := bytes.Index(b, []byte("\n--- "))
		b[headerEnd/2] ^= 1
		return b
	})
	_, err := newC(t).VerifyTarZstd(context.Background(), bad)
	if err == nil {
		t.Fatal("a damaged recipient stanza verified")
	}
	if errors.Is(err, compression.ErrNotDecryptable) {
		t.Fatalf("damage reported as a wrong key: %v", err)
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("error does not say corrupt: %v", err)
	}
}

// A nil compressor (Dependencies built without Archives) fails cleanly.
func TestNilCompressorFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	archive := buildArchive(t, dir)
	var c *compression.Compressor
	if _, err := c.VerifyTarZstd(context.Background(), archive); err == nil {
		t.Fatal("a nil compressor verified an archive")
	}
	src := filepath.Join(dir, "src")
	if _, err := c.CreateTarZstd(context.Background(), src, filepath.Join(dir, "b"+compression.EncryptedSuffix), nil); err == nil {
		t.Fatal("a nil compressor wrote an archive")
	}
}
