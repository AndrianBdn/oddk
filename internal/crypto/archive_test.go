package crypto_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/andrianbdn/oddk/internal/crypto"
)

// TestArchiveKeyGoldenVector pins the derivation. If this fails, every archive
// written so far no longer decrypts with its master key: do not "fix" the
// expected value, find what changed in NewArchiveKey.
func TestArchiveKeyGoldenVector(t *testing.T) {
	k, err := crypto.NewArchiveKey(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	const want = "AGE-SECRET-KEY-PQ-17SXN7QZZ57LGGVWQMPRF9PRT7V6TKEZD5Y80GNNWH8TDCSKHUU9SJUEF2V"
	if got := k.AgeSecret(); got != want {
		t.Fatalf("derived age secret changed:\n got %s\nwant %s", got, want)
	}
	if k.ID() != "ebbad97f" {
		t.Fatalf("key ID = %q, want ebbad97f", k.ID())
	}
}

// The key ID is the checksum at the end of the key file's line, so an operator
// can match an archive to a key file by eye.
func TestKeyIDMatchesKeyFileChecksum(t *testing.T) {
	key := bytes.Repeat([]byte{0xa5}, 32)
	path := filepath.Join(t.TempDir(), "master.key")
	if err := crypto.WriteKeyFile(path, key); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSpace(string(raw)), ";")
	if got := crypto.KeyID(key); got != fields[len(fields)-1] {
		t.Fatalf("KeyID = %q, key file checksum = %q", got, fields[len(fields)-1])
	}
}

func TestNewArchiveKeyRejectsWrongLength(t *testing.T) {
	if _, err := crypto.NewArchiveKey(make([]byte, 16)); err == nil {
		t.Fatal("a 16-byte master key was accepted")
	}
}

func mustKey(t *testing.T, fill byte) *crypto.ArchiveKey {
	t.Helper()
	k, err := crypto.NewArchiveKey(bytes.Repeat([]byte{fill}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func encryptTo(t *testing.T, r age.Recipient, plaintext string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, plaintext); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestArchiveKeyRoundTrip(t *testing.T) {
	k := mustKey(t, 1)
	ct := encryptTo(t, k.Recipient(), "payload")

	r, err := age.Decrypt(bytes.NewReader(ct), k.Identity())
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "payload" {
		t.Fatalf("decrypt = %q, %v", got, err)
	}

	// The text form must be what the stock age CLI accepts.
	parsed, err := age.ParseHybridIdentity(k.AgeSecret())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := age.Decrypt(bytes.NewReader(ct), parsed); err != nil {
		t.Fatalf("AgeSecret does not decrypt: %v", err)
	}
}

// The probe names the key a file needs, and only when no real key opens it.
func TestKeyIDProbe(t *testing.T) {
	a, b := mustKey(t, 1), mustKey(t, 2)
	ct := encryptTo(t, a.Recipient(), "x")

	probe := &crypto.KeyIDProbe{}
	_, err := age.Decrypt(bytes.NewReader(ct), b.Identity(), probe)
	if _, ok := errors.AsType[*age.NoIdentityMatchError](err); !ok {
		t.Fatalf("wrong key: got %v, want NoIdentityMatchError", err)
	}
	if probe.ID != a.ID() {
		t.Fatalf("probe recorded %q, want %q", probe.ID, a.ID())
	}

	// Listed first, it is still not what opens the file, and records nothing.
	probe = &crypto.KeyIDProbe{}
	if _, err := age.Decrypt(bytes.NewReader(ct), probe, a.Identity()); err != nil {
		t.Fatalf("right key with probe: %v", err)
	}
	if probe.ID != "" {
		t.Fatalf("probe ran although the key opened the file (recorded %q)", probe.ID)
	}

	// A plain age file has no key-ID stanza.
	other, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	probe = &crypto.KeyIDProbe{}
	if _, err := age.Decrypt(bytes.NewReader(encryptTo(t, other.Recipient(), "x")), b.Identity(), probe); err == nil {
		t.Fatal("wrong key decrypted")
	}
	if probe.ID != "" {
		t.Fatalf("plain age file reported key ID %q", probe.ID)
	}
}

// The key-ID stanza is under the header MAC: editing it breaks decryption.
func TestKeyIDStanzaIsAuthenticated(t *testing.T) {
	k := mustKey(t, 1)
	ct := encryptTo(t, k.Recipient(), "x")
	tampered := bytes.Replace(ct, []byte(" "+k.ID()+"\n"), []byte(" 00000000\n"), 1)
	if bytes.Equal(tampered, ct) {
		t.Fatal("key ID not found in the header")
	}
	if _, err := age.Decrypt(bytes.NewReader(tampered), k.Identity()); err == nil {
		t.Fatal("a header with an edited key ID still decrypted")
	}
}
