package compression_test

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/hypersequent/oddk/internal/compression"
)

// writeTarZstd builds a .tar.zst archive at archivePath whose entries are the
// given name->content map. Used to craft archives with hostile entry names that
// CreateTarZstd would never produce.
func writeTarZstd(t *testing.T, archivePath string, entries map[string]string) {
	t.Helper()

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for name, content := range entries {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o600,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write tar body %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}

	f, err := os.Create(archivePath) // #nosec G304 - controlled test path
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer func() { _ = f.Close() }()

	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatalf("new zstd writer: %v", err)
	}
	if _, err := zw.Write(tarBuf.Bytes()); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
}

// TestExtractTarZstd_RejectsTraversal verifies that extraction refuses archive
// entries that escape the destination directory, including the sibling-prefix
// case ("../<dest>-evil/...") that a naive HasPrefix(destDir) check let through.
func TestExtractTarZstd_RejectsTraversal(t *testing.T) {
	cases := []struct {
		name      string
		entryName string
		// escapePath is the absolute path the entry would write to if the guard
		// failed; the test asserts nothing was created there.
		escapeRel string
	}{
		{name: "sibling-prefix", entryName: "../extracted-evil/payload", escapeRel: "extracted-evil/payload"},
		{name: "parent-traversal", entryName: "../../oddk-evil-marker", escapeRel: "../oddk-evil-marker"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			archivePath := filepath.Join(tmpDir, "evil.tar.zst")
			extractDir := filepath.Join(tmpDir, "extracted")

			writeTarZstd(t, archivePath, map[string]string{tc.entryName: "pwned"})

			err := compression.NewCompressor().ExtractTarZstd(context.Background(), archivePath, extractDir)
			if err == nil {
				t.Fatalf("expected extraction to reject traversal entry %q, got nil error", tc.entryName)
			}

			escapePath := filepath.Join(extractDir, tc.escapeRel)
			if _, statErr := os.Stat(escapePath); statErr == nil {
				t.Fatalf("traversal entry escaped to %s despite the guard", escapePath)
			}
		})
	}
}

// TestExtractTarZstd_AllowsNestedEntries verifies the traversal guard doesn't
// reject legitimate nested paths (the backup archive layout).
func TestExtractTarZstd_AllowsNestedEntries(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "ok.tar.zst")
	extractDir := filepath.Join(tmpDir, "extracted")

	entries := map[string]string{
		"globals.sql":          "-- roles",
		"databases/mydb/3.dat": "data",
		"databases.json":       "[]",
	}
	writeTarZstd(t, archivePath, entries)

	if err := compression.NewCompressor().ExtractTarZstd(context.Background(), archivePath, extractDir); err != nil {
		t.Fatalf("ExtractTarZstd rejected a legitimate archive: %v", err)
	}

	for name, want := range entries {
		got, err := os.ReadFile(filepath.Join(extractDir, name)) // #nosec G304 - controlled test path
		if err != nil {
			t.Errorf("expected extracted file %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("content mismatch for %s: got %q, want %q", name, got, want)
		}
	}
}

func TestCompressor_CreateTarZstd(t *testing.T) {
	// Create temp directory with test files - no cleanup needed, t.TempDir() handles it
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	archivePath := filepath.Join(tmpDir, "test.tar.zst")

	// Create source directory with multiple test files and subdirectories
	if err := os.MkdirAll(sourceDir, 0o750); err != nil { // #nosec G301 - test directory
		t.Fatalf("failed to create source dir: %v", err)
	}

	// Create a subdirectory
	subDir := filepath.Join(sourceDir, "subdir")
	if err := os.MkdirAll(subDir, 0o750); err != nil { // #nosec G301 - test directory
		t.Fatalf("failed to create subdir: %v", err)
	}

	// Create multiple test files with different content
	testFiles := map[string]string{
		"test.txt":          "test content for compression",
		"data.json":         `{"test": true, "compression": "zstd"}`,
		"subdir/nested.txt": "nested file content",
		"large.txt":         string(make([]byte, 1024)), // 1KB file for better compression testing
	}

	for fileName, content := range testFiles {
		filePath := filepath.Join(sourceDir, fileName)
		if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil { // #nosec G306 - test file
			t.Fatalf("failed to create test file %s: %v", fileName, err)
		}
	}

	// Create compressor and archive
	compressor := compression.NewCompressor()
	size, err := compressor.CreateTarZstd(context.Background(), sourceDir, archivePath)
	if err != nil {
		t.Fatalf("CreateTarZstd failed: %v", err)
	}

	// Verify archive was created and has reasonable size
	if size <= 0 {
		t.Errorf("expected archive size > 0, got %d", size)
	}

	// Verify archive size is reasonable (should be compressed)
	stat, err := os.Stat(archivePath)
	if err != nil {
		t.Fatalf("failed to stat archive: %v", err)
	}

	actualSize := stat.Size()
	if actualSize != size {
		t.Errorf("returned size (%d) doesn't match actual file size (%d)", size, actualSize)
	}

	// Verify the archive is smaller than the original content (compression working)
	originalSize := int64(0)
	for _, content := range testFiles {
		originalSize += int64(len(content))
	}

	if actualSize >= originalSize {
		t.Logf("Original content: %d bytes, Archive: %d bytes", originalSize, actualSize)
		t.Logf("Note: Small test files may not compress well, this is expected")
	}

	// Verify the file has zstd magic bytes (frame header)
	archiveData, err := os.ReadFile(archivePath) // #nosec G304 - archivePath is controlled test path
	if err != nil {
		t.Fatalf("failed to read archive: %v", err)
	}

	if len(archiveData) < 4 {
		t.Errorf("archive too small to contain zstd header")
	} else {
		// Zstd magic number is 0xFD2FB528 (little endian: 28 B5 2F FD)
		zstdMagic := []byte{0x28, 0xB5, 0x2F, 0xFD}
		if !bytes.Equal(archiveData[:4], zstdMagic) {
			t.Errorf("archive doesn't start with zstd magic bytes, got: %x", archiveData[:4])
		}
	}

	// Test round-trip: extract and verify content
	extractDir := filepath.Join(tmpDir, "extracted")
	if err := compressor.ExtractTarZstd(context.Background(), archivePath, extractDir); err != nil {
		t.Fatalf("ExtractTarZstd failed: %v", err)
	}

	// Verify all original files were extracted correctly
	// Files should be directly in extractDir (not under "source/" subdirectory)
	for fileName, expectedContent := range testFiles {
		extractedPath := filepath.Join(extractDir, fileName)
		actualContent, err := os.ReadFile(extractedPath) // #nosec G304 - controlled test path
		if err != nil {
			t.Errorf("failed to read extracted file %s: %v", fileName, err)
			continue
		}

		if string(actualContent) != expectedContent {
			t.Errorf("content mismatch for %s: got %q, want %q", fileName, string(actualContent), expectedContent)
		}
	}
}

func TestCompressor_NewCompressor(t *testing.T) {
	compressor := compression.NewCompressor()
	if compressor == nil {
		t.Errorf("NewCompressor returned nil")
	}
}

func TestCompressor_CreateTarZstd_EmptyDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "empty")
	archivePath := filepath.Join(tmpDir, "empty.tar.zst")

	// Create empty directory
	if err := os.MkdirAll(sourceDir, 0o750); err != nil { // #nosec G301 - test directory
		t.Fatalf("failed to create empty dir: %v", err)
	}

	compressor := compression.NewCompressor()
	size, err := compressor.CreateTarZstd(context.Background(), sourceDir, archivePath)
	if err != nil {
		t.Fatalf("CreateTarZstd failed for empty directory: %v", err)
	}

	// Even empty tar.zst should have some size (headers)
	if size <= 0 {
		t.Errorf("expected archive size > 0 even for empty directory, got %d", size)
	}
}

func TestCompressor_CreateTarZstd_InvalidPath(t *testing.T) {
	compressor := compression.NewCompressor()

	// Test with non-existent source directory
	_, err := compressor.CreateTarZstd(context.Background(), "/non/existent/path", "/tmp/test.tar.zst")
	if err == nil {
		t.Errorf("expected error for non-existent source directory")
	}

	// Test with invalid archive path (directory that doesn't exist)
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	if err := os.MkdirAll(sourceDir, 0o750); err != nil { // #nosec G301 - test directory
		t.Fatalf("failed to create source dir: %v", err)
	}

	_, err = compressor.CreateTarZstd(context.Background(), sourceDir, "/non/existent/dir/archive.tar.zst")
	if err == nil {
		t.Errorf("expected error for invalid archive path")
	}
}
