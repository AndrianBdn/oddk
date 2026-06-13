package crypto_test

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/hypersequent/oddk/internal/crypto"
)

func TestPasswordEncryptionDecryption(t *testing.T) {
	// Generate a test key
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("Failed to generate key: %v", err)
	}

	password := "test-password-123"

	// Encrypt
	encrypted, err := crypto.EncryptPassword(password, key)
	if err != nil {
		t.Fatalf("Failed to encrypt password: %v", err)
	}

	// Should be different from original
	if encrypted == password {
		t.Errorf("Encrypted password should not equal original")
	}

	// New encryptions use the self-describing 3ncr.org/1 format
	if !strings.HasPrefix(encrypted, crypto.ThreeNcrPrefix) {
		t.Errorf("Encrypted password should have the %s prefix, got: %s", crypto.ThreeNcrPrefix, encrypted)
	}
	if crypto.IsLegacyCiphertext(encrypted) {
		t.Errorf("Fresh ciphertext should not be classified as legacy")
	}

	// Decrypt
	decrypted, err := crypto.DecryptPassword(encrypted, key)
	if err != nil {
		t.Fatalf("Failed to decrypt password: %v", err)
	}

	// Should match original
	if decrypted != password {
		t.Errorf("Decrypted password doesn't match original. Got: %s, Want: %s", decrypted, password)
	}
}

func TestPasswordEncryptionWithWrongKey(t *testing.T) {
	// Generate two different keys
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	if _, err := rand.Read(key1); err != nil {
		t.Fatalf("Failed to generate key1: %v", err)
	}
	if _, err := rand.Read(key2); err != nil {
		t.Fatalf("Failed to generate key2: %v", err)
	}

	password := "secret-password"

	// Encrypt with key1
	encrypted, err := crypto.EncryptPassword(password, key1)
	if err != nil {
		t.Fatalf("Failed to encrypt password: %v", err)
	}

	// Try to decrypt with key2 (should fail)
	_, err = crypto.DecryptPassword(encrypted, key2)
	if err == nil {
		t.Errorf("Decryption should fail with wrong key")
	}
}

// legacyEncrypt reproduces the format ODDK <= 0.1.28 stored (via cryptopasta):
// base64url(nonce || ciphertext || tag) from AES-256-GCM.
func legacyEncrypt(t *testing.T, password string, key []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	return base64.URLEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(password), nil))
}

func TestDecryptLegacyFormat(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("Failed to generate key: %v", err)
	}

	password := "pre-0.1.29-password"
	legacy := legacyEncrypt(t, password, key)

	if !crypto.IsLegacyCiphertext(legacy) {
		t.Errorf("legacy ciphertext should be classified as legacy")
	}

	decrypted, err := crypto.DecryptPassword(legacy, key)
	if err != nil {
		t.Fatalf("Failed to decrypt legacy ciphertext: %v", err)
	}
	if decrypted != password {
		t.Errorf("Legacy decryption mismatch. Got: %s, Want: %s", decrypted, password)
	}

	// Wrong key must fail on the legacy path too
	wrongKey := make([]byte, 32)
	if _, err := rand.Read(wrongKey); err != nil {
		t.Fatalf("Failed to generate wrong key: %v", err)
	}
	if _, err := crypto.DecryptPassword(legacy, wrongKey); err == nil {
		t.Errorf("Legacy decryption should fail with wrong key")
	}
}

func TestDecryptGarbageFails(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("Failed to generate key: %v", err)
	}
	for _, garbage := range []string{"", "AAAA", "3ncr.org/1#notbase64!!", "not-encrypted-at-all"} {
		if _, err := crypto.DecryptPassword(garbage, key); err == nil {
			t.Errorf("Decrypting %q should fail", garbage)
		}
	}
}
