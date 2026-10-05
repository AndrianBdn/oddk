package compression

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/mholt/archives"
)

// Compressor provides methods for creating and extracting compressed archives
type Compressor struct{}

// NewCompressor creates a new Compressor instance
func NewCompressor() *Compressor {
	return &Compressor{}
}

// Member describes one entry found inside an archive by VerifyTarZstd.
type Member struct {
	Name  string
	Size  int64
	IsDir bool
}

// VerifyTarZstd reads an archive back and proves the byte stream is internally
// consistent, returning the members it found.
//
// WHY THIS IS NOT BUILT ON archives.Extractor: the library's extract path CANNOT
// DETECT CORRUPTION AT ALL. Measured against this package's own writer, with 40
// random single-bit flips in the middle of a 4 MB archive:
//
//	ExtractTarZstd (the restore path) missed 40/40, and all 40 silently
//	extracted DIFFERENT data with err == nil.
//	This drain-based check missed 0/40.
//
// The reason is that a tar reader stops at the tar zero-block terminator, which
// sits before the end of the zstd frame — so the frame's content checksum, the
// only thing that can detect a flipped bit, is never read. Draining the
// decompressor to EOF after the tar walk is what forces that check. This is the
// entire integrity mechanism; nothing else substitutes for it.
//
// Entry bodies are deliberately NOT read. *zstd.Decoder implements neither
// io.Seeker nor io.ReaderAt, so tar's body skip already pushes every byte
// through the decompressor — reading them adds code and buys no coverage.
//
// WHAT IT PROVES: the stream decompresses, every tar header parses, and the zstd
// content checksum matches. That covers truncation, a short final write, a
// dropped Close error, and bit rot present at read time.
//
// WHAT IT DOES NOT PROVE:
//   - That the archive holds the RIGHT things. A zero-length file verifies clean
//     with zero members, and a pg_dump of an empty database verifies clean too.
//     Callers must assert the member set they expect — see the verify callback on
//     the writers.
//   - That the MEDIUM holds it. Immediately after a write the read is served from
//     the page cache; fsync narrows this but does not close it. Detecting later
//     bit rot needs a separate scrub, which is why this is exported.
func (c *Compressor) VerifyTarZstd(_ context.Context, archivePath string) ([]Member, error) {
	return verifyTarZstd(archivePath, nil)
}

// Verified is what VerifyTarZstdDigest learned about an archive.
type Verified struct {
	Members []Member
	// SHA256 is the lowercase hex digest of the archive FILE (the compressed
	// bytes, not the content), in the form `sha256sum` prints, so an operator
	// can check a copy by hand.
	SHA256 string
}

// VerifyTarZstdDigest is VerifyTarZstd plus a SHA-256 of the file, computed
// from the same read — the verify pass already pulls every byte of the file
// through the decompressor, so hashing them on the way costs no extra I/O.
// Returns nil and an error when verification fails: a corrupt archive has no
// digest worth recording.
//
// The frame checksum and the digest answer different questions. The frame
// checksum says "this file is internally broken"; it cannot say whether two
// intact copies (local, S3, a DR host's download) are the SAME archive. A
// digest recorded when the archive was written can — that is why the
// catalogue stores it (migration 022).
//
// Only the write and download paths want a digest; the restore paths call
// VerifyTarZstd and pay nothing for it.
func (c *Compressor) VerifyTarZstdDigest(_ context.Context, archivePath string) (*Verified, error) {
	h := sha256.New()
	members, err := verifyTarZstd(archivePath, h)
	if err != nil {
		return nil, err
	}
	return &Verified{Members: members, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// verifyTarZstd is the body of both verifiers. When h is non-nil, every byte
// read from the file is also written to it, in order.
//
// The cost of h, measured on a 194 MB archive on a CPU without SHA extensions:
// verify 0.47s, verify+digest 0.81s. Hashing in a separate goroutine measured
// the same, so it is done inline. CPUs with SHA extensions hash several times
// faster.
func verifyTarZstd(archivePath string, h hash.Hash) ([]Member, error) {
	f, err := os.Open(archivePath) // #nosec G304 - archivePath is controlled by caller
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	return verifyTarZstdFrom(f, h)
}

// verifyTarZstdFrom is verifyTarZstd over an already-open reader, so that
// extraction can verify and extract through ONE file handle (see
// ExtractTarZstdWith). It reads f to EOF and does not close it.
func verifyTarZstdFrom(f io.Reader, h hash.Hash) ([]Member, error) {
	src := f
	if h != nil {
		// The decoder reads src from its own goroutine, so h is written there.
		// That is safe to read afterwards only because zr.Close — explicit
		// below, and deferred on every error path, both of which run before
		// the caller calls Sum — waits for that goroutine to finish (it drains
		// the goroutine's output channel until the goroutine closes it).
		src = io.TeeReader(f, h)
	}

	zr, err := zstd.NewReader(src)
	if err != nil {
		return nil, fmt.Errorf("open zstd stream: %w", err)
	}
	defer zr.Close() // idempotent

	var members []Member
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return members, fmt.Errorf("read tar entry %d: %w", len(members), err)
		}
		members = append(members, Member{
			Name:  hdr.Name,
			Size:  hdr.Size,
			IsDir: hdr.FileInfo().IsDir(),
		})
	}

	// THE integrity check: consume whatever follows the tar terminator so zstd
	// validates the frame checksum. Without this the whole function is theatre.
	if _, err := io.Copy(io.Discard, zr); err != nil {
		return members, fmt.Errorf("archive is corrupt (zstd stream did not validate): %w", err)
	}

	if h != nil {
		// Stop the decoder's reader goroutine, then hash whatever it did not
		// read. The decoder has already seen EOF (it must, to know no further
		// frame follows), so the tail is normally empty — it is read so the
		// digest covers the whole file by construction rather than by an
		// assumption about the decoder's buffering.
		zr.Close()
		if _, err := io.Copy(io.Discard, src); err != nil {
			return members, fmt.Errorf("read archive tail: %w", err)
		}
	}

	return members, nil
}

// ErrArchiveChanged marks an archive file that was modified in place while it
// was being verified and extracted.
var ErrArchiveChanged = errors.New("archive changed during extraction")

// afterVerifyHook, when set, runs between verification and extraction. Tests
// use it to replace the archive in exactly that window; it is nil otherwise.
var afterVerifyHook func()

// ErrDigestMismatch marks an archive that verified as intact but whose SHA-256
// is not the one its catalogue recorded: a different archive under the
// expected name.
var ErrDigestMismatch = errors.New("archive digest does not match the catalogue")

// Written describes an archive published by writeVerifiedArchive.
type Written struct {
	Size int64
	// SHA256 of the published file, from the read-back that verified it — so it
	// describes bytes that were proven intact, not bytes the writer believed it
	// wrote.
	SHA256 string
}

// CreateTarZstd creates a tar.zst archive from the source directory.
// The archive contains only the contents of sourceDir, not the directory itself.
//
// verify, if non-nil, receives the members read back out of the finished archive
// and may reject it — see writeVerifiedArchive.
func (c *Compressor) CreateTarZstd(ctx context.Context, sourceDir, archivePath string, verify func([]Member) error) (Written, error) {
	// Build file map from directory contents (not the directory itself)
	// This ensures archive contains "globals.sql" not ".tmp-xxx/globals.sql"
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return Written{}, fmt.Errorf("failed to read source directory: %w", err)
	}

	fileMap := make(map[string]string)
	for _, entry := range entries {
		srcPath := filepath.Join(sourceDir, entry.Name())
		fileMap[srcPath] = entry.Name() // Map source path to archive name (relative)
	}

	files, err := archives.FilesFromDisk(ctx, nil, fileMap)
	if err != nil {
		return Written{}, fmt.Errorf("failed to read files from disk: %w", err)
	}

	return writeVerifiedArchive(ctx, c, archivePath, files, verify)
}

// ArchiveEntry maps a path on disk to the name it takes inside an archive.
// A directory source is added recursively under ArchiveName.
type ArchiveEntry struct {
	SourcePath  string
	ArchiveName string
}

// CreateTarZstdOrdered creates a tar.zst archive whose top-level entries appear
// in the order given, rather than in the map-iteration order CreateTarZstd
// produces.
//
// Order is not cosmetic here: tar has no index, so reading one member means
// decompressing everything ahead of it. A snapshot puts its manifest first so a
// reader can validate version compatibility from the first few kilobytes
// instead of streaming through gigabytes of dumps to find it.
func (c *Compressor) CreateTarZstdOrdered(ctx context.Context, entries []ArchiveEntry, archivePath string, verify func([]Member) error) (Written, error) {
	if len(entries) == 0 {
		return Written{}, fmt.Errorf("no entries to archive")
	}

	// FilesFromDisk takes a map, so its output order is undefined. Call it once
	// per entry and concatenate to get a deterministic sequence.
	var files []archives.FileInfo
	for _, entry := range entries {
		got, err := archives.FilesFromDisk(ctx, nil, map[string]string{entry.SourcePath: entry.ArchiveName})
		if err != nil {
			return Written{}, fmt.Errorf("read %s from disk: %w", entry.SourcePath, err)
		}
		files = append(files, got...)
	}

	return writeVerifiedArchive(ctx, c, archivePath, files, verify)
}

// ExtractTarZstd extracts a tar.zst archive to the destination directory using Go.
//
// It VERIFIES THE ARCHIVE FIRST, before creating anything on disk. That ordering
// is deliberate: the extractor underneath cannot detect corruption on its own
// (see VerifyTarZstd for the measurement — 40/40 single-bit flips silently
// extracted different data with err == nil), and every restore path in ODDK goes
// through here: `snapshot apply`, `snapshot restore-instance`, `backup restore`
// and `instance major-upgrade`. Verifying up front turns "restored the wrong
// bytes and reported success" into "refused before touching the destination".
//
// The cost is one extra decompression pass, with no disk writes. That is cheap
// next to writing the extracted tree, and cheap next to restoring a corrupt one.
func (c *Compressor) ExtractTarZstd(ctx context.Context, archivePath, destDir string) error {
	return c.ExtractTarZstdWith(ctx, archivePath, destDir, ExtractOptions{})
}

// ExtractOptions narrows or strengthens an extraction.
type ExtractOptions struct {
	// Keep, when non-nil, selects the members written (by their name in the
	// archive). The WHOLE archive is still verified first — integrity is a
	// property of the file, not of the members one happens to want — but a
	// restore that needs one instance out of a multi-instance snapshot no
	// longer needs disk for all of them. Directories are filtered the same
	// way, so a caller keeping a subtree must accept the subtree's own
	// directory entries (a prefix test does).
	Keep func(name string) bool

	// WantSHA256, when non-empty, is the digest a catalogue recorded when the
	// archive was written. It is checked in the same verification pass, so it
	// costs a hash and no extra read. An INTACT archive with a different
	// digest is refused: the frame check cannot see a file that was replaced
	// by another valid archive, and restoring it would put somebody else's
	// data back and report success.
	WantSHA256 string
}

// ExtractTarZstdWith is ExtractTarZstd with options; see ExtractOptions.
func (c *Compressor) ExtractTarZstdWith(ctx context.Context, archivePath, destDir string, opts ExtractOptions) error {
	// ONE handle for verification and extraction, and the SAME BYTES proven
	// for both. Verifying the path and reopening it to extract restored
	// whatever was at the path a moment later (a sync job, a `cp` into the
	// backup dir mid-restore). One descriptor makes a rename over the path
	// irrelevant; hashing the bytes each pass reads, and requiring the two
	// digests to match, closes the rest: a file rewritten in place between or
	// during the passes — even to the same length with its mtime put back,
	// which a size/mtime check (the previous guard here) cannot see.
	archiveFile, err := os.Open(archivePath) // #nosec G304 - archivePath is controlled by caller
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = archiveFile.Close() }()

	verifyHash := sha256.New()
	if _, err := verifyTarZstdFrom(archiveFile, verifyHash); err != nil {
		return fmt.Errorf("refusing to extract %s: %w", filepath.Base(archivePath), err)
	}
	verified := hex.EncodeToString(verifyHash.Sum(nil))
	if opts.WantSHA256 != "" && verified != opts.WantSHA256 {
		return fmt.Errorf("%w: refusing to extract %s: it is intact but is NOT the archive that was catalogued — "+
			"its SHA-256 is %s, the catalogue recorded %s when the archive was written. "+
			"The file has been replaced since; do not restore from it without finding out why",
			ErrDigestMismatch, filepath.Base(archivePath), verified, opts.WantSHA256)
	}
	if afterVerifyHook != nil {
		afterVerifyHook()
	}

	// Create destination directory if it doesn't exist
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	if _, err := archiveFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind archive: %w", err)
	}
	format, _, err := archives.Identify(ctx, archivePath, archiveFile)
	if err != nil {
		return fmt.Errorf("failed to identify archive format: %w", err)
	}
	// Reset file position after identification
	if _, err := archiveFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to reset file position: %w", err)
	}

	extractor, ok := format.(archives.Extractor)
	if !ok {
		return fmt.Errorf("unsupported format for extraction")
	}

	// Hash exactly the bytes extraction consumes. The decoder reads from its
	// own goroutine; Extract returns only after its deferred Close, which waits
	// for that goroutine, so the hash is complete and unshared when the tail is
	// drained below — the same ordering verifyTarZstdFrom relies on.
	extractHash := sha256.New()
	extractSrc := io.TeeReader(archiveFile, extractHash)
	keep := opts.Keep
	err = extractor.Extract(ctx, extractSrc, func(_ context.Context, f archives.FileInfo) error {
		if keep != nil && !keep(f.NameInArchive) {
			return nil
		}
		return c.handleFile(f, destDir)
	})
	if err != nil {
		return fmt.Errorf("failed to extract archive: %w", err)
	}
	// The tar reader stops at its terminator, before the end of the file; the
	// rest must be hashed too, or two files differing only past that point
	// would compare equal.
	if _, err := io.Copy(io.Discard, extractSrc); err != nil {
		return fmt.Errorf("read archive tail: %w", err)
	}

	// The bytes extracted must be the bytes verified. Callers extract into a
	// staging directory they remove on error and do nothing destructive before
	// extraction returns, so refusing here costs only the attempt.
	if extracted := hex.EncodeToString(extractHash.Sum(nil)); extracted != verified {
		return fmt.Errorf("%w: %s changed while it was being verified and extracted "+
			"(SHA-256 %s verified, %s extracted); the extracted files are discarded. Retry once nothing is writing to it",
			ErrArchiveChanged, filepath.Base(archivePath), verified, extracted)
	}

	return nil
}

// handleFile processes individual files during extraction
func (c *Compressor) handleFile(f archives.FileInfo, destDir string) error {
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

	reader, openErr := f.Open()
	if openErr != nil {
		_ = outFile.Close()
		return fmt.Errorf("open file: %w", openErr)
	}
	defer func() { _ = reader.Close() }()

	// The Close error is CHECKED. This is the READ-side twin of the bug that
	// made "completed" a claim about the writer function rather than about the
	// bytes: every restore in ODDK — snapshot apply, restore-instance, backup
	// restore, major-upgrade — reads its dumps and cluster tarballs out of here,
	// so a write error reported at close (the usual way ENOSPC and
	// network-filesystem failures surface) would hand pg_restore, or a container
	// volume, a silently truncated file and report success.
	_, copyErr := io.Copy(outFile, reader)
	if closeErr := outFile.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return fmt.Errorf("extract %s: %w", name, copyErr)
	}

	return nil
}

// writeVerifiedArchive writes an archive atomically, durably, and only after
// proving it can be read back.
//
// The sequence is the one internal/crypto/keyfile.go already uses for the master
// key — temp file -> fsync -> read back -> rename -> fsync dir — because all
// three of its steps fix a real, measured defect here:
//
//   - READ BACK. The library's Archive() writes the tar trailer and the zstd
//     epilogue inside deferred Closes whose errors it discards, so it returns nil
//     even when the tail of the archive never reached the file. Measured: a
//     writer cut short at 256 of 464 bytes still produced err == nil. "Completed"
//     meant only that the writer function returned nil; now it means the bytes
//     were read back and the zstd frame checksum matched.
//   - FSYNC. Nothing in this repo used to fsync an archive, so a catalogued
//     snapshot could point at bytes the kernel had not committed. fsync is also
//     where a deferred ENOSPC or EIO is actually reported.
//   - TEMP + RENAME. Writing straight to the final path leaves a truncated file
//     AT the real name if the process dies. The temp uses the ".tmp-" prefix
//     because the daemon's startup sweep already reaps that
//     (staleBackupArtifactPrefixes in internal/daemon/reconcile.go); inventing a
//     new prefix would leak after a crash. The temp lives in the destination
//     directory, so this needs no second copy of the data — peak disk usage is
//     unchanged.
//
// On ANY failure the temp is removed and nothing appears at archivePath. That is
// deliberate: the startup sweep only REPORTS unreferenced archives and never
// deletes them (a user may have parked one for `backup restore --file`), so a
// retained corrupt archive would be permanent litter in the directory an
// operator greps during a disaster, indistinguishable by name from a real one.
func writeVerifiedArchive(ctx context.Context, c *Compressor, archivePath string, files []archives.FileInfo, verify func([]Member) error) (Written, error) {
	dir := filepath.Dir(archivePath)

	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(archivePath)+"-*")
	if err != nil {
		return Written{}, fmt.Errorf("failed to create archive file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }

	// 0600, tighter than os.Create's 0644: archives hold database contents and
	// role password hashes.
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return Written{}, fmt.Errorf("chmod archive: %w", err)
	}

	format := archives.CompressedArchive{
		Compression: archives.Zstd{},
		Archival:    archives.Tar{},
	}
	if err := format.Archive(ctx, tmp, files); err != nil {
		cleanup()
		return Written{}, fmt.Errorf("failed to create archive: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		cleanup()
		return Written{}, fmt.Errorf("fsync archive: %w", err)
	}
	// Checked, NOT deferred: a dropped Close error is exactly how a short final
	// write becomes a silently truncated archive.
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return Written{}, fmt.Errorf("close archive: %w", err)
	}

	verified, err := c.VerifyTarZstdDigest(ctx, tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return Written{}, fmt.Errorf("archive failed verification and was discarded: %w", err)
	}
	if verify != nil {
		if err := verify(verified.Members); err != nil {
			_ = os.Remove(tmpPath)
			return Written{}, fmt.Errorf("archive failed verification and was discarded: %w", err)
		}
	}

	stat, err := os.Stat(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return Written{}, fmt.Errorf("failed to get archive size: %w", err)
	}

	if err := os.Rename(tmpPath, archivePath); err != nil {
		_ = os.Remove(tmpPath)
		return Written{}, fmt.Errorf("failed to publish archive: %w", err)
	}

	// Best effort: the rename is already durable enough on the common
	// filesystems, and failing a good archive over a directory fsync would be
	// the wrong trade.
	if d, err := os.Open(dir); err == nil { // #nosec G304 - dir is the archive's own directory
		_ = d.Sync()
		_ = d.Close()
	}

	return Written{Size: stat.Size(), SHA256: verified.SHA256}, nil
}
