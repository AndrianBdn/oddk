package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

const KeyFileSize = 32 // 256 bits for AES-256

// GetOrCreateKeyFile gets or creates the master key file
func GetOrCreateKeyFile(dataDir string) ([]byte, error) {
	keyPath := filepath.Join(dataDir, "master.key")

	// Lstat first — refuse to follow symlinks (so a swap can't substitute
	// an attacker-controlled file) and verify perms haven't been loosened
	// since creation.
	info, err := os.Lstat(keyPath)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("key file %s is a symlink, refusing to use", keyPath)
		}
		if perm := info.Mode().Perm(); perm != 0o600 && perm != 0o400 {
			return nil, fmt.Errorf("key file %s has insecure permissions %#o, expected 0600 — run: chmod 600 %s", keyPath, perm, keyPath)
		}

		keyData, err := os.ReadFile(keyPath) //nolint:gosec // keyPath is safely constructed from dataDir
		if err != nil {
			return nil, fmt.Errorf("read key file: %w", err)
		}
		// Decode from base64url
		key, err := base64.URLEncoding.DecodeString(string(keyData))
		if err != nil {
			return nil, fmt.Errorf("invalid key file format: %w", err)
		}
		if len(key) != KeyFileSize {
			return nil, fmt.Errorf("invalid key file size: got %d, expected %d", len(key), KeyFileSize)
		}
		return key, nil
	}

	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat key file: %w", err)
	}

	key := make([]byte, KeyFileSize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	// Encode as base64url for storage
	encodedKey := base64.URLEncoding.EncodeToString(key)

	err = os.WriteFile(keyPath, []byte(encodedKey), 0o600)
	if err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}

	return key, nil
}
