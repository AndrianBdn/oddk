package operations

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/operr"
	s3service "github.com/andrianbdn/oddk/internal/services/s3"
	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
)

// Copy check statuses reported by VerifySnapshot. The first four mean the copy
// is usable; every other status is a problem the operator must look at.
const (
	// CopyVerified: read in full, intact, and its SHA-256 matches the catalogue.
	CopyVerified = "verified"
	// CopyIntact: read in full and intact, but the row has no catalogued digest
	// (written before migration 022), so it cannot be proven the same archive.
	CopyIntact = "intact"
	// CopyPresent: the S3 object exists with the catalogued size; not read.
	CopyPresent = "present"
	// CopyRemoved: retention or an operator removed the copy while it was being
	// checked, and the catalogue no longer lists it. Not a problem.
	CopyRemoved = "removed"

	CopyMissing   = "missing"   // the catalogue lists it, but it is not there
	CopyMismatch  = "mismatch"  // intact, but NOT the archive that was catalogued
	CopyDamaged   = "damaged"   // fails verification, or the wrong size
	CopyUnchecked = "unchecked" // could not be checked: no key, no access, network
)

// SnapshotCopyCheck is the outcome for one copy of a snapshot.
type SnapshotCopyCheck struct {
	Location string `json:"location"`
	Status   string `json:"status"`
	// SHA256 of the bytes read, for a full read that got far enough to have one.
	SHA256 string `json:"sha256,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Problem reports whether the copy needs the operator's attention.
func (c *SnapshotCopyCheck) Problem() bool {
	if c == nil {
		return false
	}
	switch c.Status {
	case CopyVerified, CopyIntact, CopyPresent, CopyRemoved:
		return false
	}
	return true
}

// VerifySnapshotResult describes every copy of one snapshot. A copy the
// catalogue does not list is nil.
type VerifySnapshotResult struct {
	ID              int                `json:"id"`
	Filename        string             `json:"filename,omitempty"`
	CatalogueSHA256 string             `json:"catalogueSha256,omitempty"`
	Local           *SnapshotCopyCheck `json:"local,omitempty"`
	Remote          *SnapshotCopyCheck `json:"remote,omitempty"`
	OK              bool               `json:"ok"`
}

// VerifySnapshot scrubs the stored copies of a snapshot, so bit rot or a
// replaced file is found now rather than at restore time.
//
// The local copy is always read in full: decrypted, decompressed to its end,
// hashed against the catalogue, and its members asserted against its own
// manifest. The S3 copy is checked by a HEAD (exists, catalogued size) unless
// deep is set, in which case it is streamed through the same verifier without
// touching the disk. That costs a full download, so it is opt-in.
//
// It writes nothing, so it runs under ExecuteRead and does not wait behind a
// snapshot. A NULL digest is reported, never filled in: a read must not write.
// The price of running unlocked is that retention may remove a copy mid-check;
// that is re-read from the catalogue and reported as CopyRemoved, not as damage.
func VerifySnapshot(ctx context.Context, deps *Dependencies, id int, deep bool) (*VerifySnapshotResult, error) {
	record, err := deps.Store.Snapshot.Get(id)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, operr.NotFoundf("snapshot %d not found", id)
	}

	result := &VerifySnapshotResult{
		ID:              record.ID,
		Filename:        record.Filename,
		CatalogueSHA256: record.SHA256Str,
	}

	if record.LocalPath != "" {
		check := verifyLocalSnapshotCopy(ctx, deps.Archives, record.LocalPath, record.SHA256Str)
		result.Local = &check
	}
	if record.RemotePath != "" {
		check := verifyRemoteSnapshotCopy(ctx, deps, record.RemotePath, record.SHA256Str, record.Size, deep)
		result.Remote = &check
	}

	// Only a problem is worth a second look at the catalogue.
	if result.Local.Problem() || result.Remote.Problem() {
		if now, err := deps.Store.Snapshot.Get(id); err == nil {
			settleRemovedCopies(result, record, now)
		}
	}

	result.OK = !result.Local.Problem() && !result.Remote.Problem()
	return result, nil
}

// settleRemovedCopies turns a problem into CopyRemoved when the catalogue,
// re-read as now (nil when the row is gone), no longer lists that copy as it
// did in before.
func settleRemovedCopies(result *VerifySnapshotResult, before, now *snapshotstore.Record) {
	if now == nil {
		now = &snapshotstore.Record{}
	}
	settle := func(check *SnapshotCopyCheck, was, is string) {
		if check.Problem() && is != was {
			check.Status = CopyRemoved
			check.SHA256 = ""
			check.Detail = "removed from the catalogue while it was being checked"
		}
	}
	settle(result.Local, before.LocalPath, now.LocalPath)
	settle(result.Remote, before.RemotePath, now.RemotePath)
}

// verifyLocalSnapshotCopy reads the archive at path in full. wantSHA256 is the
// catalogue's digest, empty when unknown.
func verifyLocalSnapshotCopy(ctx context.Context, c *compression.Compressor, path, wantSHA256 string) SnapshotCopyCheck {
	verified, err := c.VerifyTarZstdDigest(ctx, path)
	if errors.Is(err, fs.ErrNotExist) {
		return SnapshotCopyCheck{
			Location: path,
			Status:   CopyMissing,
			Detail:   "the catalogue lists this file but it does not exist",
		}
	}
	check := fromVerification(SnapshotCopyCheck{Location: path}, verified, err, wantSHA256)

	// The members are only worth asserting against the manifest when there is
	// no digest: a matching digest already proves these are the bytes that
	// passed the same assertion when the archive was written.
	if check.Status == CopyIntact {
		manifest, err := ReadSnapshotManifestFromArchive(c, path)
		if err == nil {
			err = assertSnapshotMembers(verified.Members, manifest.Instances)
		}
		if err != nil {
			check.Status = CopyDamaged
			check.Detail = fmt.Sprintf("the archive is intact but is not a complete snapshot: %v", err)
		}
	}
	return check
}

// verifyRemoteSnapshotCopy checks the S3 copy at location: by HEAD, or with
// deep by streaming it through the verifier.
func verifyRemoteSnapshotCopy(ctx context.Context, deps *Dependencies, location, wantSHA256 string, wantSize int64, deep bool) SnapshotCopyCheck {
	check := SnapshotCopyCheck{Location: location, Status: CopyUnchecked}

	settings, err := GetActiveOffsiteSettingsDecrypted(deps)
	if err != nil {
		check.Detail = fmt.Sprintf("read offsite settings: %v", err)
		return check
	}
	if settings == nil {
		check.Detail = "offsite is not configured, so the S3 copy cannot be reached"
		return check
	}
	key, err := parseRemoteS3Location(location, settings.Bucket)
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	client, err := s3service.NewClient(ctx, settings)
	if err != nil {
		check.Detail = fmt.Sprintf("create S3 client: %v", err)
		return check
	}
	return checkRemoteObject(ctx, deps.Archives, client, client.RelativeKey(key), check, wantSHA256, wantSize, deep)
}

// checkRemoteObject is verifyRemoteSnapshotCopy past the settings lookup, so it
// can be tested against a stub S3 server.
func checkRemoteObject(ctx context.Context, c *compression.Compressor, client *s3service.Client, key string, check SnapshotCopyCheck, wantSHA256 string, wantSize int64, deep bool) SnapshotCopyCheck {
	info, err := client.StatFile(ctx, key)
	if err != nil {
		check.Status = CopyUnchecked
		check.Detail = err.Error()
		return check
	}
	if info == nil {
		check.Status = CopyMissing
		check.Detail = "the catalogue lists this object but it is not in the bucket"
		return check
	}
	if wantSize > 0 && info.Size != wantSize {
		check.Status = CopyDamaged
		check.Detail = fmt.Sprintf("the object is %d bytes but the archive was %d bytes when it was written: it was truncated or replaced",
			info.Size, wantSize)
		return check
	}
	if !deep {
		check.Status = CopyPresent
		check.Detail = "exists with the catalogued size; not read (--deep downloads and verifies it)"
		return check
	}

	pr, pw := io.Pipe()
	downloaded := make(chan error, 1)
	go func() {
		_, err := client.DownloadFileTo(ctx, key, pw)
		_ = pw.CloseWithError(err)
		downloaded <- err
	}()
	verified, verifyErr := c.VerifyTarZstdDigestFrom(pr)
	// The verifier may stop early on damage; unblock the download so it ends.
	_ = pr.CloseWithError(errVerifyStopped)
	downloadErr := <-downloaded

	if downloadErr != nil && !errors.Is(downloadErr, errVerifyStopped) {
		// The transport failed, so the verifier's complaint says nothing about
		// the object itself.
		check.Status = CopyUnchecked
		check.Detail = fmt.Sprintf("download failed: %v", downloadErr)
		return check
	}
	check = fromVerification(check, verified, verifyErr, wantSHA256)
	// Without a digest, structure is the only evidence left. The stream has no
	// manifest to hand, so this is the part of assertSnapshotMembers that needs
	// none: members present, manifest first, a non-empty oddk.db.
	if check.Status == CopyIntact {
		if err := assertSnapshotMembers(verified.Members, nil); err != nil {
			check.Status = CopyDamaged
			check.Detail = fmt.Sprintf("the archive is intact but is not a snapshot: %v", err)
		}
	}
	return check
}

var errVerifyStopped = errors.New("verification finished")

// classifyVerification sorts a verifier outcome into a copy status. Downloads
// (verifyDownloadedArchive) use it too, so a download and a scrub cannot
// disagree about what counts as damage.
func classifyVerification(verified *compression.Verified, err error, wantSHA256 string) string {
	switch {
	case errors.Is(err, compression.ErrNotDecryptable):
		return CopyUnchecked // the copy may be fine; this host's key cannot open it
	case errors.As(err, new(*fs.PathError)):
		return CopyUnchecked // the file could not be read, which says nothing about its bytes
	case err != nil:
		return CopyDamaged
	case wantSHA256 == "":
		return CopyIntact
	case verified.SHA256 != wantSHA256:
		return CopyMismatch
	}
	return CopyVerified
}

func digestMismatchDetail(got, want string) string {
	return fmt.Sprintf("its SHA-256 is %s, the catalogue recorded %s when the archive was written", got, want)
}

// fromVerification fills check from a verifier outcome.
func fromVerification(check SnapshotCopyCheck, verified *compression.Verified, err error, wantSHA256 string) SnapshotCopyCheck {
	check.Status = classifyVerification(verified, err, wantSHA256)
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	check.SHA256 = verified.SHA256
	switch check.Status {
	case CopyMismatch:
		check.Detail = "intact, but NOT the archive that was catalogued: " + digestMismatchDetail(check.SHA256, wantSHA256) +
			". Do not restore from it without finding out why"
	case CopyIntact:
		check.Detail = "intact; no digest was catalogued for this snapshot, so it cannot be matched to the original"
	}
	return check
}
