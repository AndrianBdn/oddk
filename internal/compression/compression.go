package compression

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
	"github.com/mholt/archives"

	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/operr"
)

// PlainSuffix names archives written before encryption (ODDK <= 0.1.93), and
// EncryptedSuffix names everything written since. Readers tell the two apart
// by content, never by name; the name only tells an operator which tool to
// reach for. See internal-docs/archive-encryption.md.
const (
	PlainSuffix     = ".tar.zst"
	EncryptedSuffix = ".tar.zst.age"
)

// HasArchiveSuffix reports whether name carries either archive suffix.
func HasArchiveSuffix(name string) bool {
	return strings.HasSuffix(name, PlainSuffix) || strings.HasSuffix(name, EncryptedSuffix)
}

// Compressor creates, verifies and extracts archives.
//
// Every archive it writes is age-encrypted to keys[0]. Every key is tried when
// reading, so a restore can supply both its own key and a foreign archive's.
// With no keys it can still read plaintext archives, and refuses everything
// else: there is deliberately no way to write a plaintext archive.
type Compressor struct {
	keys       []*crypto.ArchiveKey
	identities []age.Identity
}

// NewCompressor returns a Compressor for the given archive keys. The first is
// the one archives are written to.
func NewCompressor(keys ...*crypto.ArchiveKey) *Compressor {
	c := &Compressor{keys: keys}
	for _, k := range keys {
		c.identities = append(c.identities, k.Identity())
	}
	return c
}

// ErrNotDecryptable marks an encrypted archive that none of the supplied keys
// opens: the wrong master key, or none at all. It is refused before anything
// is created on disk, and is tagged operr.ErrInvalid (HTTP 400) here, at its
// origin.
var ErrNotDecryptable = errors.New("archive cannot be decrypted with the supplied master key")

// errNoCompressor is a nil *Compressor — a Dependencies built without
// Archives — failing cleanly instead of panicking.
var errNoCompressor = errors.New("no archive compressor configured (Dependencies.Archives is nil)")

// ageMagic opens every age file; zstd opens with 28 b5 2f fd.
var ageMagic = []byte("age-encryption.org/v1\n")

// hasAgeMagic reports whether r starts with the age magic. A file shorter than
// the magic is simply not encrypted.
func hasAgeMagic(r io.Reader) (bool, error) {
	head := make([]byte, len(ageMagic))
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	return bytes.Equal(head[:n], ageMagic), nil
}

// IsEncrypted reports whether the file at path is an age-encrypted archive.
func IsEncrypted(path string) (bool, error) {
	f, err := os.Open(path) // #nosec G304 - path is controlled by caller
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	return hasAgeMagic(f)
}

// openPayload returns the zstd stream inside an archive, decrypting it when the
// archive is encrypted. rest is what a caller drains to reach the end of src,
// so a hash teed under src covers the whole file.
//
// age authenticates every 64 KiB chunk, flags the last one, and rejects
// trailing data, so for an encrypted archive a flipped bit, a truncation or an
// appended tail fails here or in the payload reads, before zstd sees it. The
// zstd drain the callers already do is what reads the payload to its end.
func (c *Compressor) openPayload(src io.Reader) (payload, rest io.Reader, err error) {
	if c == nil {
		return nil, nil, errNoCompressor
	}
	br := bufio.NewReader(src)
	if magic, _ := br.Peek(len(ageMagic)); !bytes.Equal(magic, ageMagic) {
		return br, br, nil
	}
	// The probe is consulted only when every real key has failed, and then
	// records the key ID the header names (see crypto.KeyIDProbe).
	probe := &crypto.KeyIDProbe{}
	r, err := age.Decrypt(br, append(slices.Clone(c.identities), probe)...)
	if err != nil {
		if _, ok := errors.AsType[*age.NoIdentityMatchError](err); ok {
			return nil, nil, c.notDecryptable(probe.ID)
		}
		return nil, nil, fmt.Errorf("archive is corrupt (encrypted header did not validate): %w", err)
	}
	return payloadReader{r}, br, nil
}

// payloadReader keeps age's errors from being read as an end of stream. age
// reports a payload cut off before its first chunk as io.ErrUnexpectedEOF,
// and the zstd decoder, finding that before its first frame, takes it for an
// empty stream: an encrypted archive truncated right after its header then
// "verified" with zero members. Any error but io.EOF is re-made without %w, so
// no reader underneath can mistake it for one.
type payloadReader struct{ r io.Reader }

func (p payloadReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		err = fmt.Errorf("archive is corrupt (encrypted payload did not validate): %v", err) //nolint:errorlint // deliberately not wrapped, see payloadReader
	}
	return n, err
}

// notDecryptable explains a refusal in terms of key IDs, the checksum that ends
// each master.key line, so the operator can tell which key file is needed.
// need is the key ID the header names, "" if none.
func (c *Compressor) notDecryptable(need string) error {
	needs := "a different master key"
	if need != "" {
		needs = fmt.Sprintf("master key %s (the master.key whose line ends in ;%s)", need, need)
	}
	if len(c.keys) == 0 {
		return operr.Invalidf("%w: the archive is encrypted with %s and no master key was supplied", ErrNotDecryptable, needs)
	}
	have := make([]string, len(c.keys))
	for i, k := range c.keys {
		have[i] = k.ID()
		// The header names a key we hold, yet that key does not open it: the
		// recipient stanza is damaged. Reporting "wrong key" here would send
		// the operator hunting for a key file they already supplied.
		if need != "" && k.ID() == need {
			return fmt.Errorf("archive is corrupt: its header names master key %s, which was supplied, but does not open with it (the encrypted header is damaged)", need)
		}
	}
	return operr.Invalidf("%w: the archive is encrypted with %s; the supplied key is %s",
		ErrNotDecryptable, needs, strings.Join(have, ", "))
}

// tarOf opens the tar stream inside a zstd payload. The caller must Close zr:
// it waits for the decoder's goroutine, which every hash ordering here relies on.
func tarOf(payload io.Reader) (tr *tar.Reader, zr *zstd.Decoder, err error) {
	zr, err = zstd.NewReader(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("open zstd stream: %w", err)
	}
	return tar.NewReader(zr), zr, nil
}

// openTar is openPayload then tarOf: the one decrypt → decompress → tar chain
// every reader in this package goes through.
func (c *Compressor) openTar(src io.Reader) (tr *tar.Reader, zr *zstd.Decoder, rest io.Reader, err error) {
	payload, rest, err := c.openPayload(src)
	if err != nil {
		return nil, nil, nil, err
	}
	tr, zr, err = tarOf(payload)
	if err != nil {
		return nil, nil, nil, err
	}
	return tr, zr, rest, nil
}

// nextEntry is tr.Next with the policy every walk shares: io.EOF (checked with
// errors.Is) ends it, and pax global headers are not entries.
func nextEntry(tr *tar.Reader) (*tar.Header, error) {
	for {
		hdr, err := tr.Next()
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeXGlobalHeader {
			return hdr, nil
		}
	}
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
	return c.verifyTarZstd(archivePath, nil)
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
	members, err := c.verifyTarZstd(archivePath, h)
	if err != nil {
		return nil, err
	}
	return &Verified{Members: members, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// VerifyTarZstdDigestFrom is VerifyTarZstdDigest over a stream, so a remote
// copy can be checked without staging it on disk. It reads r to EOF on success;
// on failure it may stop early, and the caller must unblock its producer.
func (c *Compressor) VerifyTarZstdDigestFrom(r io.Reader) (*Verified, error) {
	h := sha256.New()
	members, err := c.verifyTarZstdFrom(r, h)
	if err != nil {
		return nil, err
	}
	return &Verified{Members: members, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// verifyTarZstd is the body of both verifiers. When h is non-nil, every byte
// read from the file is also written to it, in order.
//
// The hash covers the FILE bytes, which for an encrypted archive are the
// ciphertext: the digest identifies a copy, and the catalogue, ErrDigestMismatch
// and ErrArchiveChanged keep their meaning unchanged.
//
// The cost of h, measured on a 194 MB archive on a CPU without SHA extensions:
// verify 0.47s, verify+digest 0.81s. Hashing in a separate goroutine measured
// the same, so it is done inline. CPUs with SHA extensions hash several times
// faster.
func (c *Compressor) verifyTarZstd(archivePath string, h hash.Hash) ([]Member, error) {
	f, err := os.Open(archivePath) // #nosec G304 - archivePath is controlled by caller
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	return c.verifyTarZstdFrom(f, h)
}

// verifyTarZstdFrom is verifyTarZstd over an already-open reader, so that
// extraction can verify and extract through ONE file handle (see
// ExtractTarZstdWith). It reads f to EOF and does not close it.
func (c *Compressor) verifyTarZstdFrom(f io.Reader, h hash.Hash) ([]Member, error) {
	src := f
	if h != nil {
		// The decoder reads src from its own goroutine, so h is written there.
		// That is safe to read afterwards only because zr.Close — explicit
		// below, and deferred on every error path, both of which run before
		// the caller calls Sum — waits for that goroutine to finish (it drains
		// the goroutine's output channel until the goroutine closes it). The
		// age reader in between decrypts synchronously, in that same goroutine.
		src = io.TeeReader(f, h)
	}

	tr, zr, rest, err := c.openTar(src)
	if err != nil {
		return nil, err
	}
	defer zr.Close() // idempotent

	var members []Member
	for {
		hdr, err := nextEntry(tr)
		if errors.Is(err, io.EOF) {
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
		if _, err := io.Copy(io.Discard, rest); err != nil {
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
	files, err := filesFromDir(ctx, sourceDir)
	if err != nil {
		return Written{}, err
	}
	return writeVerifiedArchive(ctx, c, archivePath, files, verify)
}

// filesFromDir lists the contents of dir (not dir itself) for archiving, so
// the archive contains "globals.sql" rather than ".tmp-xxx/globals.sql".
func filesFromDir(ctx context.Context, dir string) ([]archives.FileInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read source directory: %w", err)
	}
	fileMap := make(map[string]string)
	for _, entry := range entries {
		fileMap[filepath.Join(dir, entry.Name())] = entry.Name()
	}
	files, err := archives.FilesFromDisk(ctx, nil, fileMap)
	if err != nil {
		return nil, fmt.Errorf("failed to read files from disk: %w", err)
	}
	return files, nil
}

// tarZstd is the inner format of every archive, encrypted or not.
var tarZstd = archives.CompressedArchive{
	Compression: archives.Zstd{},
	Archival:    archives.Tar{},
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
	archiveFile, err := os.Open(archivePath) // #nosec G304 - archivePath is controlled by caller
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = archiveFile.Close() }()

	return c.verifiedPass(archiveFile, filepath.Base(archivePath), "extract", opts.WantSHA256, func(payload io.Reader) error {
		// Created only now, after verification: a refused archive leaves no
		// destination behind.
		if err := os.MkdirAll(destDir, 0o750); err != nil {
			return fmt.Errorf("failed to create destination directory: %w", err)
		}
		tr, zr, err := tarOf(payload)
		if err != nil {
			return err
		}
		defer zr.Close() // idempotent
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			hdr, err := nextEntry(tr)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("failed to extract archive: %w", err)
			}
			if opts.Keep != nil && !opts.Keep(hdr.Name) {
				continue
			}
			if err := c.handleEntry(hdr, tr, destDir); err != nil {
				return fmt.Errorf("failed to extract archive: handling file: %s: %w", hdr.Name, err)
			}
		}
		// The tar reader stops at its terminator, before the end of the
		// stream; the rest must be read too, or two files differing only past
		// that point would compare equal.
		if _, err := io.Copy(io.Discard, zr); err != nil {
			return fmt.Errorf("read archive tail: %w", err)
		}
		zr.Close()
		return nil
	})
}

// verifiedPass verifies the archive open at f, then rewinds and hands consume
// the payload (the zstd stream, decrypted) of a SECOND read of the same handle,
// and proves the bytes that read consumed are the bytes verified.
//
// ONE handle, and the SAME BYTES proven for both reads. Verifying the path and
// reopening it would act on whatever was at the path a moment later (a sync
// job, a `cp` into the backup dir mid-restore); one descriptor makes a rename
// over the path irrelevant. Hashing the bytes each read takes, and requiring
// the two digests to match, closes the rest: a file rewritten in place between
// or during the reads — even to the same length with its mtime put back, which
// a size/mtime check (the guard this replaced) cannot see.
//
// wantSHA256, when non-empty, is the catalogue's digest; an intact archive
// with another digest is refused before consume runs. consume must read the
// payload to its end and, if it starts a zstd decoder, Close it before
// returning, so the second digest is complete. Callers act only on what
// consume produced once this returns nil.
func (c *Compressor) verifiedPass(f *os.File, name, verb, wantSHA256 string, consume func(payload io.Reader) error) error {
	verifyHash := sha256.New()
	if _, err := c.verifyTarZstdFrom(f, verifyHash); err != nil {
		return fmt.Errorf("refusing to %s %s: %w", verb, name, err)
	}
	verified := verifyHash.Sum(nil)
	if wantSHA256 != "" && hex.EncodeToString(verified) != wantSHA256 {
		return fmt.Errorf("%w: refusing to %s %s: it is intact but is NOT the archive that was catalogued — "+
			"its SHA-256 is %x, the catalogue recorded %s when the archive was written. "+
			"The file has been replaced since; do not restore from it without finding out why",
			ErrDigestMismatch, verb, name, verified, wantSHA256)
	}
	if afterVerifyHook != nil {
		afterVerifyHook()
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind archive: %w", err)
	}
	passHash := sha256.New()
	payload, rest, err := c.openPayload(io.TeeReader(f, passHash))
	if err != nil {
		return fmt.Errorf("refusing to %s %s: %w", verb, name, err)
	}
	if err := consume(payload); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, rest); err != nil {
		return fmt.Errorf("read archive tail: %w", err)
	}
	if read := passHash.Sum(nil); !bytes.Equal(read, verified) {
		return fmt.Errorf("%w: %s changed while it was being verified and read "+
			"(SHA-256 %x verified, %x read); nothing it produced may be used. Retry once nothing is writing to it",
			ErrArchiveChanged, name, verified, read)
	}
	return nil
}

// handleEntry writes one tar entry under destDir; body is the entry's content.
func (c *Compressor) handleEntry(hdr *tar.Header, body io.Reader, destDir string) error {
	name := hdr.Name
	if name == "" {
		return nil
	}
	info := hdr.FileInfo()

	// Reject non-regular, non-directory entries (symlinks, hardlinks, devices,
	// fifos). ODDK archives only ever contain regular files and directories;
	// honoring other entry types during extraction would be a traversal vector.
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported archive entry %q (mode %v)", name, info.Mode())
	}

	// Secure the path against directory traversal. A naive
	// strings.HasPrefix(filePath, destDir) check is bypassable by a sibling
	// prefix such as "../<destdir-basename>-evil/..." (which still HasPrefix
	// destDir); resolve a relative path from destDir and reject any entry that
	// escapes it.
	filePath := filepath.Join(destDir, name) // #nosec G305 - escape is refused just below
	rel, err := filepath.Rel(destDir, filePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("invalid archive entry path: %s", name)
	}

	if info.IsDir() {
		return os.MkdirAll(filePath, 0o750)
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o750); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	outFile, err := os.Create(filePath) // #nosec G304 - filePath is secured above
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", filePath, err)
	}

	// The Close error is CHECKED. This is the READ-side twin of the bug that
	// made "completed" a claim about the writer function rather than about the
	// bytes: every restore in ODDK — snapshot apply, restore-instance, backup
	// restore, major-upgrade — reads its dumps and cluster tarballs out of here,
	// so a write error reported at close (the usual way ENOSPC and
	// network-filesystem failures surface) would hand pg_restore, or a container
	// volume, a silently truncated file and report success.
	_, copyErr := io.Copy(outFile, body)
	if closeErr := outFile.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return fmt.Errorf("extract %s: %w", name, copyErr)
	}

	return nil
}

// writeVerifiedArchive writes an encrypted archive atomically, durably, and
// only after proving it can be read back — decrypted, decompressed and walked.
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
	// Never a plaintext archive: they hold every database's contents and the
	// role password hashes, and are shipped to S3.
	if c == nil {
		return Written{}, errNoCompressor
	}
	if len(c.keys) == 0 {
		return Written{}, errors.New("no archive key: refusing to write an unencrypted archive")
	}
	if !strings.HasSuffix(archivePath, EncryptedSuffix) {
		return Written{}, fmt.Errorf("archive path %q must end in %s", filepath.Base(archivePath), EncryptedSuffix)
	}
	dir := filepath.Dir(archivePath)

	tmp, cleanup, err := createPrivateTemp(dir, filepath.Base(archivePath))
	if err != nil {
		return Written{}, err
	}
	tmpPath := tmp.Name()

	enc, err := age.Encrypt(tmp, c.keys[0].Recipient())
	if err != nil {
		cleanup()
		return Written{}, fmt.Errorf("start archive encryption: %w", err)
	}
	if err := tarZstd.Archive(ctx, enc, files); err != nil {
		cleanup()
		return Written{}, fmt.Errorf("failed to create archive: %w", err)
	}
	// Checked, NOT deferred: Close writes the final, flagged chunk. Without it
	// the file is a truncated stream (the read-back below would refuse it).
	if err := enc.Close(); err != nil {
		cleanup()
		return Written{}, fmt.Errorf("finish archive encryption: %w", err)
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

	syncDir(dir)
	return Written{Size: stat.Size(), SHA256: verified.SHA256}, nil
}

// ErrMemberNotFound marks an archive that does not hold the member asked for.
var ErrMemberNotFound = errors.New("member not found in archive")

// ReadMember returns the content of the member called name, streaming the
// archive only as far as it — which is why snapshots put their manifest first.
// At most limit bytes are read.
//
// This is a peek, not a verification: it proves nothing about the rest of the
// file. What it does read of an encrypted archive is authenticated chunk by
// chunk, and the wrong key is refused (ErrNotDecryptable) exactly as a full
// verification would refuse it.
func (c *Compressor) ReadMember(archivePath, name string, limit int64) ([]byte, error) {
	f, err := os.Open(archivePath) // #nosec G304 - archivePath is controlled by caller
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = f.Close() }()

	tr, zr, _, err := c.openTar(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	for {
		hdr, err := nextEntry(tr)
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: %s", ErrMemberNotFound, name)
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Name != name {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, limit))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		return data, nil
	}
}

// DecryptTo writes the plaintext .tar.zst inside an encrypted archive to
// outPath, for an operator who needs the archive outside ODDK. It goes through
// verifiedPass like an extraction, so what is written is what was verified.
// The output appears atomically, 0600, only on success; an existing outPath is
// refused rather than overwritten.
func (c *Compressor) DecryptTo(archivePath, outPath string) error {
	if _, err := os.Lstat(outPath); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite it", outPath)
	}
	archiveFile, err := os.Open(archivePath) // #nosec G304 - archivePath is controlled by caller
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = archiveFile.Close() }()

	name := filepath.Base(archivePath)
	if encrypted, err := hasAgeMagic(archiveFile); err != nil {
		return fmt.Errorf("read archive: %w", err)
	} else if !encrypted {
		return fmt.Errorf("%s is not encrypted; it is already a plain .tar.zst", name)
	}
	if _, err := archiveFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind archive: %w", err)
	}

	dir := filepath.Dir(outPath)
	var tmp *os.File
	cleanup := func() {}
	err = c.verifiedPass(archiveFile, name, "decrypt", "", func(payload io.Reader) error {
		var err error
		if tmp, cleanup, err = createPrivateTemp(dir, filepath.Base(outPath)); err != nil {
			return err
		}
		if _, err := io.Copy(tmp, payload); err != nil {
			return fmt.Errorf("decrypt archive: %w", err)
		}
		return nil
	})
	if err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync output: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close output: %w", err)
	}
	// Link, not rename: a rename would silently replace a file created at
	// outPath since the check above.
	if err := os.Link(tmp.Name(), outPath); err != nil {
		cleanup()
		return fmt.Errorf("publish output: %w", err)
	}
	cleanup()
	syncDir(dir)
	return nil
}

// createPrivateTemp creates a 0600 temp file in dir for an output named base.
// The ".tmp-" prefix is the one the daemon's startup sweep already reaps
// (staleBackupArtifactPrefixes in internal/daemon/reconcile.go), so a crash
// leaks nothing; living in the destination directory means no second copy of
// the data. 0600 because archives, encrypted or not, hold database contents.
// cleanup closes and removes the temp file and is safe to call more than once.
func createPrivateTemp(dir, base string) (*os.File, func(), error) {
	tmp, err := os.CreateTemp(dir, ".tmp-"+base+"-*")
	if err != nil {
		return nil, func() {}, fmt.Errorf("create temp file: %w", err)
	}
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("chmod temp file: %w", err)
	}
	return tmp, cleanup, nil
}

// syncDir flushes a directory entry after a rename or link. Best effort: the
// file itself is already fsynced, and failing a good output over a directory
// fsync would be the wrong trade.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil { // #nosec G304 - the output's own directory
		_ = d.Sync()
		_ = d.Close()
	}
}
