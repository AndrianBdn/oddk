package operations

import (
	"bytes"
	"slices"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/operr"
)

// ArchiveCompressor returns the compressor every archive goes through. It
// writes to the archive key derived from masterKey (this host's), and reads
// with that key plus any extra master keys — a foreign archive's, from
// --master-key. Nil and repeated keys are skipped; with none at all it can
// read only plaintext archives from ODDK <= 0.1.93 and refuses to write.
// See internal-docs/archive-encryption.md.
func ArchiveCompressor(masterKey []byte, extra ...[]byte) (*compression.Compressor, error) {
	var seen [][]byte
	var keys []*crypto.ArchiveKey
	for _, mk := range append([][]byte{masterKey}, extra...) {
		if mk == nil || slices.ContainsFunc(seen, func(have []byte) bool { return bytes.Equal(have, mk) }) {
			continue
		}
		seen = append(seen, mk)
		k, err := crypto.NewArchiveKey(mk)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return compression.NewCompressor(keys...), nil
}

// ArchiveCompressorFromKeyFile reads an operator-supplied master.key and
// returns the compressor for its archives, plus the key itself (callers also
// decrypt the snapshot's stored credentials with it).
func ArchiveCompressorFromKeyFile(path string) (*compression.Compressor, []byte, error) {
	key, err := crypto.ReadKeyFileAt(path)
	if err != nil {
		return nil, nil, err
	}
	c, err := ArchiveCompressor(key)
	if err != nil {
		return nil, nil, err
	}
	return c, key, nil
}

// restoreArchiveKeys resolves the keys a restore from a snapshot needs. The
// archive may be this host's (deps.MasterKey) or another's, whose master key
// the operator supplies with --master-key (a path on the daemon's
// filesystem). The archive is decrypted with either; sourceKey is the one the
// snapshot's stored credentials are decrypted with — the supplied key when
// there is one.
//
// It runs before the manifest is read, because the manifest is inside the
// encryption.
func restoreArchiveKeys(deps *Dependencies, masterKeyPath string) (c *compression.Compressor, sourceKey []byte, err error) {
	if masterKeyPath == "" {
		return deps.Archives, deps.MasterKey, nil
	}
	sourceKey, err = crypto.ReadKeyFileAt(masterKeyPath)
	if err != nil {
		return nil, nil, operr.Invalidf("read source master key: %v", err)
	}
	c, err = ArchiveCompressor(deps.MasterKey, sourceKey)
	if err != nil {
		return nil, nil, operr.Invalidf("archive key: %v", err)
	}
	return c, sourceKey, nil
}
