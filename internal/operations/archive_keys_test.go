package operations

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/compression"
)

// testMasterKey is the master key the operations tests encrypt archives to.
var testMasterKey = bytes.Repeat([]byte{9}, 32)

func testCompressor(t *testing.T) *compression.Compressor {
	t.Helper()
	c, err := ArchiveCompressor(testMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A foreign archive opens once its key is supplied alongside this host's, and
// not before.
func TestArchiveCompressorReadsWithTheExtraKey(t *testing.T) {
	foreignKey := bytes.Repeat([]byte{8}, 32)
	foreign, err := ArchiveCompressor(foreignKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o750); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "a"+compression.EncryptedSuffix)
	if _, err := foreign.CreateTarZstd(context.Background(), src, archive, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := testCompressor(t).VerifyTarZstd(context.Background(), archive); !errors.Is(err, compression.ErrNotDecryptable) {
		t.Fatalf("this host's key alone: got %v, want ErrNotDecryptable", err)
	}
	both, err := ArchiveCompressor(testMasterKey, foreignKey, testMasterKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := both.VerifyTarZstd(context.Background(), archive); err != nil {
		t.Fatalf("with the foreign key supplied: %v", err)
	}
}

func TestArchiveCompressorRejectsAMalformedKey(t *testing.T) {
	if _, err := ArchiveCompressor(make([]byte, 5)); err == nil {
		t.Fatal("a malformed master key was accepted")
	}
}
