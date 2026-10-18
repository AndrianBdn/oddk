package compression

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/andrianbdn/oddk/internal/crypto"
)

// WritePlainForTest writes a plaintext .tar.zst the way ODDK <= 0.1.93 did, so
// tests can prove those archives stay readable. Production code has no way to
// write one; this file is compiled into tests only.
func WritePlainForTest(ctx context.Context, sourceDir, archivePath string) error {
	files, err := filesFromDir(ctx, sourceDir)
	if err != nil {
		return err
	}
	f, err := os.Create(archivePath) // #nosec G304 - test path
	if err != nil {
		return err
	}
	if err := tarZstd.Archive(ctx, f, files); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// KeyForTest is the archive key the package's tests use; fill picks which one.
func KeyForTest(t *testing.T, fill byte) *crypto.ArchiveKey {
	t.Helper()
	k, err := crypto.NewArchiveKey(bytes.Repeat([]byte{fill}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}
