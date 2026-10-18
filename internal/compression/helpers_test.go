package compression_test

import (
	"context"
	"testing"

	"github.com/andrianbdn/oddk/internal/compression"
)

// newC is the compressor production uses: one archive key.
func newC(t *testing.T) *compression.Compressor {
	t.Helper()
	return compression.NewCompressor(compression.KeyForTest(t, 1))
}

// archiveKind is an archive as ODDK writes it now (encrypted) or as ODDK <=
// 0.1.93 wrote it (plain). Every integrity test runs against both: old
// archives in S3 must stay restorable, and stay checked, indefinitely.
type archiveKind struct {
	name   string
	suffix string
	write  func(t *testing.T, src, path string) error
}

var archiveKinds = []archiveKind{
	{"encrypted", compression.EncryptedSuffix, func(t *testing.T, src, path string) error {
		_, err := newC(t).CreateTarZstd(context.Background(), src, path, nil)
		return err
	}},
	{"plain", compression.PlainSuffix, func(t *testing.T, src, path string) error {
		return compression.WritePlainForTest(context.Background(), src, path)
	}},
}

func forEachKind(t *testing.T, fn func(t *testing.T, k archiveKind)) {
	for _, k := range archiveKinds {
		t.Run(k.name, func(t *testing.T) { fn(t, k) })
	}
}
