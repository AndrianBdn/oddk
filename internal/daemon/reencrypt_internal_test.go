package daemon

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypersequent/oddk/internal/crypto"
	"github.com/hypersequent/oddk/internal/store"
	"github.com/hypersequent/oddk/internal/store/offsite"
)

// legacyEncrypt reproduces the pre-0.1.29 stored format:
// base64url(nonce || ciphertext || tag) from AES-256-GCM.
func legacyEncrypt(t *testing.T, plaintext string, key []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return base64.URLEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), nil))
}

func TestReencryptLegacySecrets(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.NewStore(filepath.Join(dataDir, "oddk.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}

	masterKey := make([]byte, 32)
	if _, err := rand.Read(masterKey); err != nil {
		t.Fatal(err)
	}
	wrongKey := make([]byte, 32)
	if _, err := rand.Read(wrongKey); err != nil {
		t.Fatal(err)
	}

	// A legacy-encrypted instance password: must be upgraded in place.
	legacyPw := legacyEncrypt(t, "pg-pass", masterKey)
	if _, err := st.Instances.Create("legacy-inst", 5432, "17", legacyPw, "", 1, 1024, "default", "postgres:17"); err != nil {
		t.Fatal(err)
	}

	// An already-3ncr password: must be left byte-identical.
	freshPw, err := crypto.EncryptPassword("fresh-pass", masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Instances.Create("fresh-inst", 5433, "17", freshPw, "", 1, 1024, "default", "postgres:17"); err != nil {
		t.Fatal(err)
	}

	// A legacy ciphertext under a different master key (e.g. restored data
	// dir with the wrong key): must be left untouched, not corrupted.
	badPw := legacyEncrypt(t, "other-pass", wrongKey)
	if _, err := st.Instances.Create("badkey-inst", 5434, "17", badPw, "", 1, 1024, "default", "postgres:17"); err != nil {
		t.Fatal(err)
	}

	// A legacy-encrypted offsite secret on the active settings row.
	if err := st.Offsite.Create(&offsite.OffsiteSettings{
		Type:            offsite.TypeS3,
		Bucket:          "bucket",
		AccessKeyID:     "ak",
		SecretAccessKey: legacyEncrypt(t, "s3-secret", masterKey),
	}); err != nil {
		t.Fatal(err)
	}

	reencryptLegacySecrets(st, masterKey)

	inst, err := st.Instances.Get("legacy-inst")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(inst.Password, crypto.ThreeNcrPrefix) {
		t.Errorf("legacy instance password should be upgraded to 3ncr, got %q", inst.Password)
	}
	if got, err := crypto.DecryptPassword(inst.Password, masterKey); err != nil || got != "pg-pass" {
		t.Errorf("upgraded password should decrypt to original: got %q, err %v", got, err)
	}

	fresh, err := st.Instances.Get("fresh-inst")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Password != freshPw {
		t.Errorf("already-3ncr password should be untouched")
	}

	bad, err := st.Instances.Get("badkey-inst")
	if err != nil {
		t.Fatal(err)
	}
	if bad.Password != badPw {
		t.Errorf("undecryptable legacy password should be left as-is, got %q", bad.Password)
	}

	settings, err := st.Offsite.GetActive()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(settings.SecretAccessKey, crypto.ThreeNcrPrefix) {
		t.Errorf("offsite secret should be upgraded to 3ncr, got %q", settings.SecretAccessKey)
	}
	if got, err := crypto.DecryptPassword(settings.SecretAccessKey, masterKey); err != nil || got != "s3-secret" {
		t.Errorf("upgraded offsite secret should decrypt to original: got %q, err %v", got, err)
	}

	// Idempotent: a second run changes nothing.
	upgradedPw := inst.Password
	reencryptLegacySecrets(st, masterKey)
	inst2, err := st.Instances.Get("legacy-inst")
	if err != nil {
		t.Fatal(err)
	}
	if inst2.Password != upgradedPw {
		t.Errorf("second sweep should be a no-op")
	}
}
