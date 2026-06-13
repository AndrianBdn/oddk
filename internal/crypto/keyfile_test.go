package crypto_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypersequent/oddk/internal/crypto"
)

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
