package crypto_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/crypto"
)

// writeLegacyKey plants a pre-V1 headerless key file: a bare padded base64url
// string, exactly what ODDK <= 0.1.59 wrote.
func writeLegacyKey(t *testing.T, dir string, key []byte, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, []byte(base64.URLEncoding.EncodeToString(key)), perm); err != nil {
		t.Fatalf("plant legacy key: %v", err)
	}
	return path
}

func readKeyFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// handChecksum re-derives the documented checksum rule independently of the
// implementation: the first 4 bytes of SHA-256 over the base64url text, hex
// encoded. This is the value `printf %s '<payload>' | sha256sum | cut -c1-8`
// produces, and pinning it here is what keeps that documented command honest.
func handChecksum(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:4])
}

func TestGetOrCreateKeyFile_CreatesNewKey(t *testing.T) {
	dir := t.TempDir()

	key, err := crypto.GetOrCreateKeyFile(dir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(key) != crypto.KeyFileSize {
		t.Fatalf("key size: got %d, want %d", len(key), crypto.KeyFileSize)
	}

	info, err := os.Stat(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("created with perm %#o, want 0600", perm)
	}
}

func TestGetOrCreateKeyFile_ReadsExistingKey(t *testing.T) {
	dir := t.TempDir()

	first, err := crypto.GetOrCreateKeyFile(dir)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := crypto.GetOrCreateKeyFile(dir)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("subsequent reads returned different keys")
	}
}

func TestGetOrCreateKeyFile_RejectsLoosePerms(t *testing.T) {
	dir := t.TempDir()
	if _, err := crypto.GetOrCreateKeyFile(dir); err != nil {
		t.Fatalf("seed: %v", err)
	}
	keyPath := filepath.Join(dir, "master.key")
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := crypto.GetOrCreateKeyFile(dir)
	if err == nil {
		t.Fatal("expected error for 0644 perms, got nil")
	}
	if !strings.Contains(err.Error(), "insecure permissions") {
		t.Errorf("error message: %v", err)
	}
}

func TestGetOrCreateKeyFile_AcceptsReadOnly(t *testing.T) {
	dir := t.TempDir()
	if _, err := crypto.GetOrCreateKeyFile(dir); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "master.key"), 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := crypto.GetOrCreateKeyFile(dir); err != nil {
		t.Errorf("0400 should be accepted, got: %v", err)
	}
}

func TestGetOrCreateKeyFile_RejectsSymlink(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	if _, err := crypto.GetOrCreateKeyFile(src); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Symlink(filepath.Join(src, "master.key"), filepath.Join(dst, "master.key")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := crypto.GetOrCreateKeyFile(dst)
	if err == nil {
		t.Fatal("expected error for symlinked key, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error message: %v", err)
	}
}

// TestKeyFileOnDiskFormat pins the bytes on disk. Nothing did before this, so a
// format change used to pass the whole crypto suite unchanged — and the file is
// read by operators, by `--master-key` at disaster-recovery time, and by any
// secret scanner we point at it.
func TestKeyFileOnDiskFormat(t *testing.T) {
	dir := t.TempDir()
	key, err := crypto.GetOrCreateKeyFile(dir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	raw := readKeyFile(t, filepath.Join(dir, "master.key"))

	if !strings.HasSuffix(raw, "\n") {
		t.Errorf("key file should end with a newline, got %q", raw)
	}
	if n := strings.Count(raw, "\n"); n != 1 {
		t.Errorf("key file must be exactly one line, got %d newlines", n)
	}

	fields := strings.Split(strings.TrimSpace(raw), ";")
	if len(fields) != 4 {
		t.Fatalf("expected 4 ';'-separated fields, got %d: %q", len(fields), raw)
	}
	if fields[0] != "ODDK-SECRET-MASTER-KEY" {
		t.Errorf("marker: got %q, want ODDK-SECRET-MASTER-KEY", fields[0])
	}
	if fields[1] != "V1" {
		t.Errorf("version: got %q, want V1", fields[1])
	}
	if fields[2] != base64.URLEncoding.EncodeToString(key) {
		t.Errorf("payload is not the padded base64url of the returned key")
	}
	if fields[3] != handChecksum(fields[2]) {
		t.Errorf("checksum: got %q, want %q (first 4 bytes of SHA-256 over the payload text)",
			fields[3], handChecksum(fields[2]))
	}
	// ';' must not appear inside the payload, or the split above is ambiguous.
	if strings.Contains(fields[2], ";") {
		t.Errorf("payload contains the field separator")
	}
}

func TestKeyFileLegacyUpgradedInPlace(t *testing.T) {
	dir := t.TempDir()
	want := bytes.Repeat([]byte{0xAB}, crypto.KeyFileSize)
	path := writeLegacyKey(t, dir, want, 0o600)

	got, err := crypto.GetOrCreateKeyFile(dir)
	if err != nil {
		t.Fatalf("read legacy key: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("legacy key material changed on load")
	}

	raw := readKeyFile(t, path)
	if !strings.HasPrefix(raw, "ODDK-SECRET-MASTER-KEY;V1;") {
		t.Errorf("legacy key was not upgraded in place, file is still %q", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("upgrade changed permissions to %#o, want 0600", perm)
	}

	// The upgrade must be idempotent and must not disturb the key.
	again, err := crypto.GetOrCreateKeyFile(dir)
	if err != nil {
		t.Fatalf("reload after upgrade: %v", err)
	}
	if !bytes.Equal(again, want) {
		t.Errorf("key changed across the upgrade")
	}
	if reread := readKeyFile(t, path); reread != raw {
		t.Errorf("second load rewrote an already-upgraded file")
	}

	// No temp file may survive the rewrite.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".master.key-") {
			t.Errorf("left a temp file behind: %s", e.Name())
		}
	}
}

// Binaries <= 0.1.59 cannot parse V1, and the upgrade happens at daemon startup
// before almost anything else — exactly when an installer's automatic rollback
// is most likely to fire. The upgrade is cosmetic, so it must leave a way back.
func TestKeyFileUpgradeLeavesRollbackCopy(t *testing.T) {
	dir := t.TempDir()
	want := bytes.Repeat([]byte{0x77}, crypto.KeyFileSize)
	path := writeLegacyKey(t, dir, want, 0o600)

	if _, err := crypto.GetOrCreateKeyFile(dir); err != nil {
		t.Fatalf("read legacy key: %v", err)
	}

	backup := path + crypto.LegacyKeyFileBackupSuffix
	raw := readKeyFile(t, backup)

	// It must be byte-for-byte what a <= 0.1.59 binary writes and expects. That
	// loader does not TrimSpace, so a trailing newline here would not roll back.
	if raw != base64.URLEncoding.EncodeToString(want) {
		t.Errorf("rollback copy is not the canonical legacy form: %q", raw)
	}
	if strings.ContainsAny(raw, "\n\r ") {
		t.Errorf("rollback copy must carry no whitespace, got %q", raw)
	}
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("rollback copy perm %#o, want 0600 — it is key material", perm)
	}

	// os.WriteFile does not chmod an existing file, so a pre-existing loose-mode
	// file at that path must still end up 0600 rather than inheriting its mode.
	dir2 := t.TempDir()
	path2 := writeLegacyKey(t, dir2, want, 0o600)
	if err := os.WriteFile(path2+crypto.LegacyKeyFileBackupSuffix, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed loose backup: %v", err)
	}
	if _, err := crypto.GetOrCreateKeyFile(dir2); err != nil {
		t.Fatalf("load: %v", err)
	}
	info2, err := os.Stat(path2 + crypto.LegacyKeyFileBackupSuffix)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info2.Mode().Perm(); perm != 0o600 {
		t.Errorf("rollback copy inherited perm %#o from a pre-existing file, want 0600", perm)
	}

	// Restoring it must actually produce a working key again.
	if err := os.Rename(backup, path); err != nil {
		t.Fatalf("simulate rollback: %v", err)
	}
	got, err := crypto.ReadKeyFileAt(path)
	if err != nil {
		t.Fatalf("restored legacy key does not load: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("restored key differs from the original")
	}
}

// If the rollback copy cannot be written, the (cosmetic) upgrade must not
// happen — an irreversible rewrite of the master key is never worth taking
// blind.
func TestKeyFileUpgradeSkippedWhenRollbackCopyImpossible(t *testing.T) {
	dir := t.TempDir()
	want := bytes.Repeat([]byte{0x88}, crypto.KeyFileSize)
	path := writeLegacyKey(t, dir, want, 0o600)

	// Occupy the backup path with a directory, so writing the copy fails while
	// the key file itself stays perfectly writable.
	if err := os.Mkdir(path+crypto.LegacyKeyFileBackupSuffix, 0o700); err != nil {
		t.Fatalf("block backup path: %v", err)
	}

	got, err := crypto.GetOrCreateKeyFile(dir)
	if err != nil {
		t.Fatalf("the key must still load: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("key material changed")
	}
	if raw := readKeyFile(t, path); strings.Contains(raw, "ODDK-SECRET-MASTER-KEY") {
		t.Errorf("upgraded without a rollback copy; file is now %q", raw)
	}
}

// A 0400 key is legal to read and impossible to rewrite. The upgrade is
// cosmetic, so it must neither fail the load nor loosen the permissions.
func TestKeyFileLegacyReadOnlyNotRewritten(t *testing.T) {
	dir := t.TempDir()
	want := bytes.Repeat([]byte{0x11}, crypto.KeyFileSize)
	path := writeLegacyKey(t, dir, want, 0o400)

	got, err := crypto.GetOrCreateKeyFile(dir)
	if err != nil {
		t.Fatalf("a read-only legacy key must still load: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("key material changed")
	}
	if raw := readKeyFile(t, path); strings.Contains(raw, "ODDK-SECRET-MASTER-KEY") {
		t.Errorf("0400 key was rewritten; it should have been left alone")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o400 {
		t.Errorf("permissions changed to %#o, want 0400 left untouched", perm)
	}
}

// A corrupted payload must be reported as corruption, not as "wrong key" — the
// operator's next move is different in each case, and that distinction is the
// entire reason the checksum field exists.
func TestKeyFileChecksumMismatchIsCorruption(t *testing.T) {
	dir := t.TempDir()
	key, err := crypto.GetOrCreateKeyFile(dir)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	path := filepath.Join(dir, "master.key")
	fields := strings.Split(strings.TrimSpace(readKeyFile(t, path)), ";")

	// Transpose two payload characters: still valid base64url, still 32 bytes,
	// but a different key. Exactly the hand-copy failure the checksum is for.
	p := []byte(fields[2])
	p[0], p[1] = p[1], p[0]
	if string(p) == fields[2] {
		p[0] = 'A' ^ p[0] // the first two characters happened to match
	}
	corrupt := strings.Join([]string{fields[0], fields[1], string(p), fields[3]}, ";") + "\n"
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err = crypto.GetOrCreateKeyFile(dir)
	if err == nil {
		t.Fatal("expected a checksum failure, got nil")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error should name the checksum: %v", err)
	}
	if !strings.Contains(err.Error(), "not the same as a wrong key") {
		t.Errorf("error must distinguish corruption from a wrong key: %v", err)
	}
	_ = key
}

func TestKeyFileMalformedAndUnknownVersion(t *testing.T) {
	payload := base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, crypto.KeyFileSize))

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"missing checksum field", "ODDK-SECRET-MASTER-KEY;V1;" + payload + "\n", "malformed"},
		{"extra field", "ODDK-SECRET-MASTER-KEY;V1;" + payload + ";" + handChecksum(payload) + ";x\n", "malformed"},
		{"future version", "ODDK-SECRET-MASTER-KEY;V2;" + payload + ";" + handChecksum(payload) + "\n", "upgrade ODDK"},
		{"empty file", "\n", "is empty"},
		{"garbage payload", "ODDK-SECRET-MASTER-KEY;V1;not!base64;" + handChecksum("not!base64") + "\n", "base64url"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "master.key"), []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := crypto.GetOrCreateKeyFile(dir)
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should contain %q", err, tc.want)
			}
		})
	}
}

// GetOrCreateKeyFile used not to trim whitespace while ReadKeyFileAt did. That
// asymmetry was latent until the format grew a trailing newline; if it comes
// back, `snapshot apply --master-key` keeps working while daemon startup bricks.
func TestKeyFileReadersAgreeOnWhitespace(t *testing.T) {
	want := bytes.Repeat([]byte{0x33}, crypto.KeyFileSize)
	encoded := base64.URLEncoding.EncodeToString(want)

	for _, form := range []string{
		encoded,                 // legacy, no trailing newline
		encoded + "\n",          // legacy with a newline
		"  " + encoded + "\n\n", // legacy, sloppily copied
		"ODDK-SECRET-MASTER-KEY;V1;" + encoded + ";" + handChecksum(encoded),
		"ODDK-SECRET-MASTER-KEY;V1;" + encoded + ";" + handChecksum(encoded) + "\n",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "master.key")
		if err := os.WriteFile(path, []byte(form), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		viaDaemon, err := crypto.GetOrCreateKeyFile(dir)
		if err != nil {
			t.Errorf("GetOrCreateKeyFile rejected %q: %v", form, err)
			continue
		}
		if !bytes.Equal(viaDaemon, want) {
			t.Errorf("GetOrCreateKeyFile decoded %q to the wrong key", form)
		}

		// Same bytes through the operator-supplied path, from a fresh file so
		// the in-place upgrade above cannot mask a difference.
		other := filepath.Join(t.TempDir(), "archived.key")
		if err := os.WriteFile(other, []byte(form), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		viaOperator, err := crypto.ReadKeyFileAt(other)
		if err != nil {
			t.Errorf("ReadKeyFileAt rejected %q: %v", form, err)
			continue
		}
		if !bytes.Equal(viaOperator, want) {
			t.Errorf("ReadKeyFileAt decoded %q to the wrong key", form)
		}
	}
}

func TestWriteKeyFileEmitsCurrentFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	want := bytes.Repeat([]byte{0x44}, crypto.KeyFileSize)

	if err := crypto.WriteKeyFile(path, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	if raw := readKeyFile(t, path); !strings.HasPrefix(raw, "ODDK-SECRET-MASTER-KEY;V1;") {
		t.Errorf("WriteKeyFile emitted %q", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm %#o, want 0600", perm)
	}

	got, err := crypto.ReadKeyFileAt(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("round trip changed the key")
	}

	// Overwriting an existing key must replace it atomically, leaving no debris.
	next := bytes.Repeat([]byte{0x55}, crypto.KeyFileSize)
	if err := crypto.WriteKeyFile(path, next); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, err = crypto.ReadKeyFileAt(path)
	if err != nil {
		t.Fatalf("read back after overwrite: %v", err)
	}
	if !bytes.Equal(got, next) {
		t.Errorf("overwrite did not take effect")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected only master.key in the dir, got %d entries", len(entries))
	}
}
