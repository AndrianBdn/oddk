package crypto

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"filippo.io/age"
)

// archiveKeyInfo is the HKDF context that turns the master key into the
// archive key. Changing it changes the key every archive is encrypted with, so
// old archives would no longer decrypt: TestArchiveKeyGoldenVector pins it.
const archiveKeyInfo = "oddk archive identity v1"

// keyIDStanzaType is the age header stanza ODDK adds next to the real
// recipient stanza. It carries the key ID in plaintext so a wrong-key refusal
// can name the key the archive needs. Native age identities skip stanza types
// they do not know, so the stock age CLI still decrypts the file; the header
// MAC covers the stanza, so it cannot be altered without detection once the
// file is decrypted.
const keyIDStanzaType = "oddk-key-id"

// ArchiveKey is the age identity ODDK encrypts archives to, derived from the
// master key so disaster recovery stays one secret (internal-docs/archive-encryption.md).
type ArchiveKey struct {
	identity *age.HybridIdentity
	id       string
}

// NewArchiveKey derives the archive key from a 32-byte master key. The
// derivation is HKDF-SHA256 to a 32-byte seed, which is exactly what an age
// post-quantum (ML-KEM-768 + X25519) identity is.
func NewArchiveKey(masterKey []byte) (*ArchiveKey, error) {
	if len(masterKey) != KeyFileSize {
		return nil, fmt.Errorf("master key is %d bytes, want %d", len(masterKey), KeyFileSize)
	}
	seed, err := hkdf.Key(sha256.New, masterKey, nil, archiveKeyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("derive archive key: %w", err)
	}
	// Upper-cased: bech32 decoding is case-insensitive, but age then compares
	// the prefix to "AGE-SECRET-KEY-PQ-" exactly.
	identity, err := age.ParseHybridIdentity(strings.ToUpper(bech32Encode("AGE-SECRET-KEY-PQ-", seed)))
	if err != nil {
		return nil, fmt.Errorf("build archive key: %w", err)
	}
	return &ArchiveKey{identity: identity, id: KeyID(masterKey)}, nil
}

// KeyID names a master key in messages: the checksum that already ends its
// key-file line (`ODDK-SECRET-MASTER-KEY;V1;<payload>;<checksum>`), so an
// operator can match an archive to a key file by eye. It is 32 bits of a hash
// of a 256-bit random key, so publishing it in an archive header reveals
// nothing useful about the key.
func KeyID(masterKey []byte) string {
	return keyChecksum(base64.URLEncoding.EncodeToString(masterKey))
}

// ID is the key ID of the master key this archive key was derived from.
func (k *ArchiveKey) ID() string { return k.id }

// Identity decrypts archives encrypted to this key.
func (k *ArchiveKey) Identity() age.Identity { return k.identity }

// Recipient encrypts to this key and records its ID in the header.
func (k *ArchiveKey) Recipient() age.Recipient {
	return keyIDRecipient{inner: k.identity.Recipient(), id: k.id}
}

// AgeSecret is the identity in age's own text form (AGE-SECRET-KEY-PQ-1…), for
// `age -d -i`. It is a full decryption secret: print it only locally, to the
// data-dir owner, never over the API.
func (k *ArchiveKey) AgeSecret() string { return k.identity.String() }

// keyIDRecipient wraps the real recipient and appends the key-ID stanza.
type keyIDRecipient struct {
	inner *age.HybridRecipient
	id    string
}

func (r keyIDRecipient) Wrap(fileKey []byte) ([]*age.Stanza, error) {
	s, _, err := r.WrapWithLabels(fileKey)
	return s, err
}

// WrapWithLabels passes the post-quantum label through, so age still refuses
// to mix this recipient with a classic one.
func (r keyIDRecipient) WrapWithLabels(fileKey []byte) ([]*age.Stanza, []string, error) {
	stanzas, labels, err := r.inner.WrapWithLabels(fileKey)
	if err != nil {
		return nil, nil, err
	}
	return append(stanzas, &age.Stanza{Type: keyIDStanzaType, Args: []string{r.id}}), labels, nil
}

// KeyIDProbe is an age identity that opens nothing: it records the key ID
// stanza of the header it is shown. age tries native identities first and
// stops at the first that opens the file, so a probe listed with the real keys
// is consulted only when none of them could — the one case that needs to name
// the key. The ID is unauthenticated at that point: a hint for an error
// message, never an input to a decision.
type KeyIDProbe struct {
	// ID is the recorded key ID, or "" when the header has none (an archive
	// not written by ODDK).
	ID string
}

func (p *KeyIDProbe) Unwrap(stanzas []*age.Stanza) ([]byte, error) {
	for _, s := range stanzas {
		if s.Type == keyIDStanzaType && len(s.Args) == 1 {
			p.ID = s.Args[0]
		}
	}
	return nil, age.ErrIncorrectIdentity
}

// bech32Encode is BIP-173 bech32 (not bech32m), the encoding age uses for its
// keys; age's own encoder is internal. Unlike BIP-173, there is no 90-character
// limit, matching age. NewArchiveKey parses the result with age, which checks
// the checksum, so a bug here fails loudly rather than producing a wrong key.
func bech32Encode(hrp string, data []byte) string {
	const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	hrp = strings.ToLower(hrp)
	values := convertBits8to5(data)
	checksum := bech32Checksum(hrp, values)
	var b strings.Builder
	b.WriteString(hrp)
	b.WriteByte('1')
	for _, v := range append(values, checksum...) {
		b.WriteByte(charset[v])
	}
	return b.String()
}

func convertBits8to5(data []byte) []byte {
	var out []byte
	acc, bits := uint32(0), uint(0)
	for _, b := range data {
		acc = acc<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, byte((acc>>bits)&31))
		}
	}
	if bits > 0 {
		out = append(out, byte((acc<<(5-bits))&31))
	}
	return out
}

func bech32Checksum(hrp string, values []byte) []byte {
	expanded := make([]byte, 0, len(hrp)*2+1+len(values)+6)
	for i := range len(hrp) {
		expanded = append(expanded, hrp[i]>>5)
	}
	expanded = append(expanded, 0)
	for i := range len(hrp) {
		expanded = append(expanded, hrp[i]&31)
	}
	expanded = append(expanded, values...)
	expanded = append(expanded, 0, 0, 0, 0, 0, 0)
	mod := bech32Polymod(expanded) ^ 1
	out := make([]byte, 6)
	for i := range 6 {
		out[i] = byte((mod >> (5 * (5 - i))) & 31)
	}
	return out
}

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := range 5 {
			if (top>>i)&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}
