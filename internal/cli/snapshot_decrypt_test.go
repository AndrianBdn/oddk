package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/crypto"
)

func writeEncryptedArchive(t *testing.T, dir string, masterKey []byte) string {
	t.Helper()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "manifest.json"), []byte(`{"formatVersion":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := crypto.NewArchiveKey(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "snapshot-h-20261017000000"+compression.EncryptedSuffix)
	if _, err := compression.NewCompressor(k).CreateTarZstd(context.Background(), src, archive, nil); err != nil {
		t.Fatal(err)
	}
	return archive
}

// snapshot decrypt runs with no daemon and no CLI config at all — the DR
// situation it exists for.
func TestSnapshotDecrypt(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{3}, 32)
	keyPath := filepath.Join(dir, "master.key")
	if err := crypto.WriteKeyFile(keyPath, key); err != nil {
		t.Fatal(err)
	}
	archive := writeEncryptedArchive(t, dir, key)
	env := []string{"HOME=" + dir, "ODDK_CLI_CONFIG=" + filepath.Join(dir, "absent.json")}

	out, err := runCLI(t, env, "snapshot", "decrypt", "--file", archive, "--master-key", keyPath)
	if err != nil {
		t.Fatalf("decrypt: %v\n%s", err, out)
	}
	plain := strings.TrimSuffix(archive, ".age")
	if enc, err := compression.IsEncrypted(plain); err != nil || enc {
		t.Fatalf("output %s: encrypted=%v err=%v", plain, enc, err)
	}
	if _, err := compression.NewCompressor().VerifyTarZstd(context.Background(), plain); err != nil {
		t.Fatalf("output does not verify as a plain archive: %v", err)
	}

	// The printed identity is what the stock age CLI reads with -i.
	out, err = runCLI(t, env, "snapshot", "decrypt", "--master-key", keyPath, "--print-age-identity")
	if err != nil {
		t.Fatalf("print identity: %v", err)
	}
	ids, err := age.ParseIdentities(strings.NewReader(out))
	if err != nil {
		t.Fatalf("printed identity does not parse as an age identity file: %v\n%s", err, out)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := age.Decrypt(f, ids...); err != nil {
		t.Fatalf("printed identity does not decrypt the archive: %v", err)
	}

	// A wrong key is refused and names both keys.
	wrongPath := filepath.Join(dir, "other.key")
	if err := crypto.WriteKeyFile(wrongPath, bytes.Repeat([]byte{4}, 32)); err != nil {
		t.Fatal(err)
	}
	_, err = runCLI(t, env, "snapshot", "decrypt", "--file", archive, "--master-key", wrongPath, "--out", filepath.Join(dir, "x.tar.zst"))
	if !errors.Is(err, compression.ErrNotDecryptable) || !strings.Contains(err.Error(), crypto.KeyID(key)) {
		t.Fatalf("wrong key: %v", err)
	}
}
