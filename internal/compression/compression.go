package compression

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mholt/archives"
)

// Compressor provides methods for creating and extracting compressed archives
type Compressor struct{}

// NewCompressor creates a new Compressor instance
func NewCompressor() *Compressor {
	return &Compressor{}
}

// CreateTarZstd creates a tar.zst archive from the source directory.
// The archive contains only the contents of sourceDir, not the directory itself.
func (c *Compressor) CreateTarZstd(ctx context.Context, sourceDir, archivePath string) (int64, error) {
	// Build file map from directory contents (not the directory itself)
	// This ensures archive contains "globals.sql" not ".tmp-xxx/globals.sql"
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return 0, fmt.Errorf("failed to read source directory: %w", err)
	}

	fileMap := make(map[string]string)
	for _, entry := range entries {
		srcPath := filepath.Join(sourceDir, entry.Name())
		fileMap[srcPath] = entry.Name() // Map source path to archive name (relative)
	}

	files, err := archives.FilesFromDisk(ctx, nil, fileMap)
	if err != nil {
		return 0, fmt.Errorf("failed to read files from disk: %w", err)
	}

	outputFile, err := os.Create(archivePath) // #nosec G304 - archivePath is controlled by caller
	if err != nil {
		return 0, fmt.Errorf("failed to create archive file: %w", err)
	}
	defer func() { _ = outputFile.Close() }()

	// Define the format (tar + zstd)
	format := archives.CompressedArchive{
		Compression: archives.Zstd{},
		Archival:    archives.Tar{},
	}

	if err := format.Archive(ctx, outputFile, files); err != nil {
		return 0, fmt.Errorf("failed to create archive: %w", err)
	}

	stat, err := outputFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("failed to get archive size: %w", err)
	}

	return stat.Size(), nil
}

// ExtractTarZstd extracts a tar.zst archive to the destination directory using Go
func (c *Compressor) ExtractTarZstd(ctx context.Context, archivePath, destDir string) error {
	// Create destination directory if it doesn't exist
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	// Open the archive file
	archiveFile, err := os.Open(archivePath) // #nosec G304 - archivePath is controlled by caller
	if err != nil {
		return fmt.Errorf("failed to open archive: %w", err)
	}
	defer func() { _ = archiveFile.Close() }()

	// Identify the format
	format, _, err := archives.Identify(ctx, archivePath, archiveFile)
	if err != nil {
		return fmt.Errorf("failed to identify archive format: %w", err)
	}

	// Reset file position after identification
	if _, err := archiveFile.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to reset file position: %w", err)
	}

	extractor, ok := format.(archives.Extractor)
	if !ok {
		return fmt.Errorf("unsupported format for extraction")
	}

	err = extractor.Extract(ctx, archiveFile, func(ctx context.Context, f archives.FileInfo) error {
		return c.handleFile(ctx, f, destDir)
	})
	if err != nil {
		return fmt.Errorf("failed to extract archive: %w", err)
	}

	return nil
}

// handleFile processes individual files during extraction
func (c *Compressor) handleFile(ctx context.Context, f archives.FileInfo, destDir string) error {
	name := f.NameInArchive
	if name == "" {
		return nil
	}

	// Reject non-regular, non-directory entries (symlinks, hardlinks, devices,
	// fifos). ODDK archives only ever contain regular files and directories;
	// honoring other entry types during extraction would be a traversal vector.
	if !f.IsDir() && !f.Mode().IsRegular() {
		return fmt.Errorf("unsupported archive entry %q (mode %v)", name, f.Mode())
	}

	// Secure the path against directory traversal. A naive
	// strings.HasPrefix(filePath, destDir) check is bypassable by a sibling
	// prefix such as "../<destdir-basename>-evil/..." (which still HasPrefix
	// destDir); resolve a relative path from destDir and reject any entry that
	// escapes it.
	filePath := filepath.Join(destDir, name)
	rel, err := filepath.Rel(destDir, filePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("invalid archive entry path: %s", name)
	}

	if f.IsDir() {
		return os.MkdirAll(filePath, 0o750)
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o750); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	outFile, err := os.Create(filePath) // #nosec G304 - filePath is secured above
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", filePath, err)
	}
	defer func() { _ = outFile.Close() }()

	// Copy file contents
	reader, openErr := f.Open()
	if openErr != nil {
		return fmt.Errorf("open file: %w", openErr)
	}
	defer func() { _ = reader.Close() }()

	if _, copyErr := io.Copy(outFile, reader); copyErr != nil {
		return fmt.Errorf("copy: %w", copyErr)
	}

	return nil
}
