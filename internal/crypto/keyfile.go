package crypto

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const KeyFileSize = 32 // 256 bits for AES-256

// Master key file format, one line, LF-terminated:
//
//	ODDK-SECRET-MASTER-KEY;V1;<base64url of 32 bytes>;<checksum>
//
// The marker exists so the file identifies itself: an operator who finds it
// loose on a disk, and a secret scanner sweeping a repo or a bucket, can both
// tell what it is without knowing the path it came from. ';' is the separator
// because it cannot appear in base64url (alphabet A-Z a-z 0-9 - _, plus '='
// padding), so the split is unambiguous.
//
// The checksum is the first 4 bytes of SHA-256 over the base64url *text*, hex
// encoded. It is deliberately over the text rather than the decoded key: the
// thing that gets copied by hand is the text, so hashing it catches copy damage
// directly and stays verifiable with one portable command —
//
//	printf %s '<payload>' | sha256sum | cut -c1-8
//
// (hashing the decoded bytes would first need a decode, and GNU coreutils
// `base64 -d` rejects the base64url alphabet — it needs `basenc --base64url -d`
// or a `tr -- '-_' '+/'` — while macOS's BSD `base64` happens to accept both, so
// the same documented command would not work everywhere). It distinguishes
// "the right key, copied badly" from "the wrong key", which have different fixes.
const (
	keyFileMarker      = "ODDK-SECRET-MASTER-KEY"
	keyFileVersion     = "V1"
	keyFileFieldCount  = 4 // marker;version;payload;checksum
	keyFileChecksumLen = 8 // hex chars = 4 bytes of SHA-256
)

// acceptLegacyKeyFile controls whether the pre-V1 headerless format (a bare
// 44-character base64url string, written by ODDK <= 0.1.59) is still read.
//
// Flip to false once every deployment has restarted at least once on a binary
// that writes V1 — GetOrCreateKeyFile upgrades the file in place on load, so
// coverage is automatic. Note that operator-supplied `--master-key` files are
// archived artifacts outside our control (password managers, DR media), so
// flipping this makes an old archived key unusable until converted; the refusal
// message below prints the conversion.
const acceptLegacyKeyFile = true

// GetOrCreateKeyFile gets or creates the master key file.
//
// A legacy headerless key is upgraded to the V1 format in place, best-effort:
// the upgrade is cosmetic, so a failure to rewrite (a read-only 0400 key, a
// read-only filesystem) is logged and the already-decoded key is returned. The
// upgrade lives here rather than in a separate startup sweep because this
// function aborts daemon startup on an unrecognised file, so a sweep running
// after it would never see the file it was meant to migrate.
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
		perm := info.Mode().Perm()
		if perm != 0o600 && perm != 0o400 {
			return nil, fmt.Errorf("key file %s has insecure permissions %#o, expected 0600 — run: chmod 600 %s", keyPath, perm, keyPath)
		}

		keyData, err := os.ReadFile(keyPath) //nolint:gosec // keyPath is safely constructed from dataDir
		if err != nil {
			return nil, fmt.Errorf("read key file: %w", err)
		}
		key, legacy, err := parseKeyFile(keyData, keyPath)
		if err != nil {
			return nil, err
		}
		if legacy {
			upgradeKeyFileFormat(keyPath, key, perm)
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

	if err := writeKeyFile(keyPath, key); err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}

	return key, nil
}

// ReadKeyFileAt reads a master key from an arbitrary path, for `snapshot apply`
// where the operator supplies the source host's key from wherever they archived
// it.
//
// Unlike GetOrCreateKeyFile this never creates a key, never rewrites it, and it
// does not reject loose permissions: a key restored from backup media
// legitimately arrives 0644, and refusing it would block a disaster recovery for
// a property of a file that is about to be re-written with 0600 anyway. Symlinks
// are still refused, and the format is still validated.
func ReadKeyFileAt(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read master key %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("master key %s is a symlink, refusing to use", path)
	}

	keyData, err := os.ReadFile(path) //nolint:gosec // operator-supplied path is the point of this function
	if err != nil {
		return nil, fmt.Errorf("read master key %s: %w", path, err)
	}
	key, _, err := parseKeyFile(keyData, path)
	if err != nil {
		return nil, err
	}
	return key, nil
}

// WriteKeyFile installs a master key at path with 0600 permissions, in the
// current on-disk format.
func WriteKeyFile(path string, key []byte) error {
	if len(key) != KeyFileSize {
		return fmt.Errorf("refusing to write master key of size %d, expected %d", len(key), KeyFileSize)
	}
	return writeKeyFile(path, key)
}

// encodeKeyFile renders the on-disk representation of a key, including the
// trailing newline. Both readers trim surrounding whitespace, so the newline is
// safe and makes `cat master.key` behave.
func encodeKeyFile(key []byte) string {
	payload := base64.URLEncoding.EncodeToString(key)
	return fmt.Sprintf("%s;%s;%s;%s\n", keyFileMarker, keyFileVersion, payload, keyChecksum(payload))
}

// keyChecksum returns the first 4 bytes of SHA-256 over the base64url payload,
// hex encoded.
func keyChecksum(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:keyFileChecksumLen/2])
}

// parseKeyFile decodes either the V1 format or, while acceptLegacyKeyFile is
// set, the pre-V1 headerless one. legacy reports which was found, so the caller
// can decide whether to upgrade the file.
func parseKeyFile(raw []byte, path string) (key []byte, legacy bool, err error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, false, fmt.Errorf("master key %s is empty", path)
	}

	if !strings.HasPrefix(text, keyFileMarker+";") {
		if !acceptLegacyKeyFile {
			return nil, false, legacyKeyFileRefusal(path)
		}
		key, err := decodeKeyPayload(text, path)
		if err != nil {
			return nil, false, err
		}
		return key, true, nil
	}

	parts := strings.Split(text, ";")
	if len(parts) != keyFileFieldCount {
		return nil, false, fmt.Errorf("master key %s is malformed: expected %d %q-separated fields, got %d",
			path, keyFileFieldCount, ";", len(parts))
	}
	if parts[1] != keyFileVersion {
		return nil, false, fmt.Errorf("master key %s is format %s but this binary understands %s — upgrade ODDK",
			path, parts[1], keyFileVersion)
	}
	payload, sum := parts[2], parts[3]

	// Checksum before decode: a corrupted payload should be reported as
	// corruption, not as a base64 error, because the fix is different.
	if want := keyChecksum(payload); !strings.EqualFold(sum, want) {
		return nil, false, fmt.Errorf(
			"master key %s fails its checksum (recorded %s, computed %s): the file is corrupted or was edited. "+
				"This is not the same as a wrong key — restore this file from your key backup rather than looking for a different key",
			path, sum, want)
	}

	key, err = decodeKeyPayload(payload, path)
	if err != nil {
		return nil, false, err
	}
	return key, false, nil
}

func decodeKeyPayload(payload, path string) ([]byte, error) {
	key, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("master key %s is not a valid ODDK key file (expected base64url): %w", path, err)
	}
	if len(key) != KeyFileSize {
		return nil, fmt.Errorf("master key %s has size %d, expected %d", path, len(key), KeyFileSize)
	}
	return key, nil
}

// legacyKeyFileRefusal is the message shown once acceptLegacyKeyFile is off. It
// prints a conversion that derives the new line from the file itself rather than
// echoing the key, so the remedy is copy-pasteable without putting key material
// into a terminal scrollback, a ticket, or a log.
func legacyKeyFileRefusal(path string) error {
	return fmt.Errorf(`master key %s is in the pre-%s headerless format, which this binary no longer accepts.
The key material is unchanged — rewrite the file with a %s header:

  k=$(tr -d '[:space:]' < %s) && \
    printf '%s;%s;%%s;%%s\n' "$k" "$(printf %%s "$k" | sha256sum | cut -c1-%d)" > %s.v1 && \
    chmod 600 %s.v1 && mv %s.v1 %s

(on macOS use `+"`shasum -a 256`"+` in place of `+"`sha256sum`"+`)`,
		path, keyFileVersion, keyFileVersion, path,
		keyFileMarker, keyFileVersion, keyFileChecksumLen, path, path, path, path)
}

// writeKeyFile writes a key atomically: a torn write here would make every
// encrypted secret in oddk.db unrecoverable, and unlike the original create-only
// writer this path also runs when a key already exists (format upgrade, and
// `snapshot apply` installing the source host's key). The temp file is fully
// written, synced and *parsed back* before the rename, so nothing replaces a
// live key until the replacement has been proven readable.
func writeKeyFile(path string, key []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".master.key-*")
	if err != nil {
		return fmt.Errorf("create temp key file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp key file: %w", err)
	}
	if _, err := tmp.WriteString(encodeKeyFile(key)); err != nil {
		cleanup()
		return fmt.Errorf("write temp key file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp key file: %w", err)
	}

	// Prove the bytes on disk parse back to the key we meant to store before
	// letting them replace anything.
	verifyData, err := os.ReadFile(tmpPath) //nolint:gosec // tmpPath is created by this function
	if err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("verify temp key file: %w", err)
	}
	verify, _, err := parseKeyFile(verifyData, tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("verify temp key file: %w", err)
	}
	if !bytes.Equal(verify, key) {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("verify temp key file %s: read back a different key than was written", tmpPath)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("install key file %s: %w", path, err)
	}

	// Persist the rename itself, so a crash cannot leave the directory entry
	// pointing at a file that was never durably linked.
	if d, err := os.Open(dir); err == nil { //nolint:gosec // dir is the parent of a path this function was asked to write
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// LegacyKeyFileBackupSuffix names the pre-upgrade copy kept beside an upgraded
// master key, so a binary rollback has something to roll back to.
const LegacyKeyFileBackupSuffix = ".pre-v1"

// writeLegacyBackup stores the pre-upgrade key text and proves it landed intact.
//
// It chmods explicitly because os.WriteFile does not change the mode of a file
// that already exists — this is key material and must not inherit 0644 from
// whatever was there before. It reads back because an unverified rollback copy
// is worse than none: it would license the one-way upgrade while being unusable
// on the day it is needed.
func writeLegacyBackup(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { //nolint:gosec // path is keyPath (built from dataDir) plus a constant suffix
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // path is keyPath plus a constant suffix
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if string(got) != content {
		return fmt.Errorf("verify: read back %d bytes, wrote %d", len(got), len(content))
	}
	return nil
}

// upgradeKeyFileFormat rewrites a legacy headerless key file in the V1 format.
//
// It is best-effort by design: the key has already been read successfully, so
// failing here must not stop the daemon. A 0400 key is left alone rather than
// loosened.
//
// It also refuses to upgrade unless it can first preserve the pre-upgrade file.
// Binaries <= 0.1.59 decode master.key with a bare base64 call and cannot parse
// V1, so this rewrite is one-way — and it happens at daemon startup before
// almost anything else, which is exactly when the rollback both installers
// perform on a failed start is most likely to fire. The upgrade itself is
// cosmetic (the legacy format still loads), so it is never worth taking without
// a way back. Same rule as `snapshot apply`, which sets the displaced key aside
// as master.key.replaced-<stamp> rather than dropping it.
func upgradeKeyFileFormat(path string, key []byte, perm os.FileMode) {
	if perm == 0o400 {
		log.Printf("Warning: master key %s is in the legacy headerless format but is read-only (0400); leaving it. To upgrade: chmod 600 %s and restart", path, path)
		return
	}

	// Re-encode rather than re-read: this is byte-for-byte what a <= 0.1.59
	// binary writes and expects (no trailing newline — that loader does not trim
	// whitespace), and it avoids a second read of a file we already have.
	backupPath := path + LegacyKeyFileBackupSuffix
	legacy := base64.URLEncoding.EncodeToString(key)
	if err := writeLegacyBackup(backupPath, legacy); err != nil {
		log.Printf("Warning: could not preserve %s, so the master key is being left in the legacy format "+
			"(it loads fine; upgrading without a usable rollback copy is not worth it): %v", backupPath, err)
		return
	}

	if err := writeKeyFile(path, key); err != nil {
		log.Printf("Warning: could not upgrade master key %s to the %s format (the key itself is fine and in use): %v", path, keyFileVersion, err)
		_ = os.Remove(backupPath)
		return
	}

	log.Printf("Master key %s upgraded to the %s self-describing format (key material unchanged). "+
		"ODDK <= 0.1.59 cannot read this format; if you roll the binary back, restore it with: mv %s %s",
		path, keyFileVersion, backupPath, path)
}
