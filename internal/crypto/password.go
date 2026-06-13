package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/3ncr/tokencrypt"
)

// ThreeNcrPrefix is the header of the 3ncr.org/1 encrypted-string format that
// all new encryptions use. Stored values without it are in the legacy format
// (base64url of AES-256-GCM nonce||ciphertext||tag, as written by ODDK
// <= 0.1.28 via cryptopasta); the daemon re-encrypts those at startup, and
// DecryptPassword keeps a legacy fallback for rows that pre-date the sweep.
const ThreeNcrPrefix = "3ncr.org/1#"

// EncryptPassword encrypts a password with the master key in the 3ncr.org/1
// format (AES-256-GCM with the raw 32-byte key, self-describing header).
func EncryptPassword(password string, masterKey []byte) (string, error) {
	tc, err := newTokenCrypt(masterKey)
	if err != nil {
		return "", err
	}
	encrypted, err := tc.Encrypt3ncr(password)
	if err != nil {
		return "", fmt.Errorf("encrypt password: %w", err)
	}
	return encrypted, nil
}

// DecryptPassword decrypts a stored value with the master key, handling both
// the 3ncr.org/1 format and the legacy pre-0.1.29 format.
func DecryptPassword(encryptedPassword string, masterKey []byte) (string, error) {
	if strings.HasPrefix(encryptedPassword, ThreeNcrPrefix) {
		tc, err := newTokenCrypt(masterKey)
		if err != nil {
			return "", err
		}
		plaintext, err := tc.DecryptIf3ncr(encryptedPassword)
		if err != nil {
			return "", fmt.Errorf("decrypt password: %w", err)
		}
		return plaintext, nil
	}
	return decryptLegacy(encryptedPassword, masterKey)
}

// IsLegacyCiphertext reports whether a stored value is in the legacy format
// and should be re-encrypted (used by the daemon's startup sweep).
func IsLegacyCiphertext(stored string) bool {
	return stored != "" && !strings.HasPrefix(stored, ThreeNcrPrefix)
}

func newTokenCrypt(masterKey []byte) (*tokencrypt.EncToken, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(masterKey))
	}
	tc, err := tokencrypt.NewRawTokenCrypt(masterKey)
	if err != nil {
		return nil, fmt.Errorf("init token crypt: %w", err)
	}
	return tc, nil
}

// decryptLegacy decrypts the format written by ODDK <= 0.1.28:
// base64url(nonce || ciphertext || tag) from AES-256-GCM.
func decryptLegacy(encryptedPassword string, masterKey []byte) (string, error) {
	if len(masterKey) != 32 {
		return "", fmt.Errorf("master key must be 32 bytes, got %d", len(masterKey))
	}

	ciphertext, err := base64.URLEncoding.DecodeString(encryptedPassword)
	if err != nil {
		return "", fmt.Errorf("decode encrypted password: %w", err)
	}

	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return "", fmt.Errorf("decrypt password: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("decrypt password: %w", err)
	}
	if len(ciphertext) < gcm.NonceSize() {
		return "", fmt.Errorf("decrypt password: ciphertext too short")
	}

	plaintext, err := gcm.Open(nil,
		ciphertext[:gcm.NonceSize()],
		ciphertext[gcm.NonceSize():],
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("decrypt password: %w", err)
	}
	return string(plaintext), nil
}
