package operations

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/rfc3339time"
	s3service "github.com/andrianbdn/oddk/internal/services/s3"
	"github.com/andrianbdn/oddk/internal/store/offsite"
)

// DownloadSnapshotResult describes a retrieved archive.
type DownloadSnapshotResult struct {
	ID        int    `json:"id"`
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	LocalPath string `json:"localPath"`
}

// DownloadSnapshot pulls a snapshot back from S3 into backupDir.
//
// Without this the offsite copy is write-only: a snapshot is uploaded, local
// retention then removes the local copy, and the primary disaster-recovery
// artifact can no longer be obtained through ODDK at all — exactly when it is
// needed. `backup download` has always existed for the per-instance case; this
// is its counterpart.
func DownloadSnapshot(ctx context.Context, deps *Dependencies, id int, backupDir string) (*DownloadSnapshotResult, error) {
	settings, err := GetActiveOffsiteSettingsDecrypted(deps)
	if err != nil {
		return nil, fmt.Errorf("get offsite settings: %w", err)
	}
	if settings == nil {
		return nil, operr.Invalidf("offsite backup not configured")
	}

	record, err := deps.Store.Snapshot.Get(id)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, operr.NotFoundf("snapshot %d not found", id)
	}
	if record.RemotePath == "" {
		return nil, operr.Invalidf("snapshot %d has no offsite copy to download", id)
	}

	localPath := filepath.Join(backupDir, record.Filename)
	if record.LocalPath != "" {
		if _, statErr := os.Stat(record.LocalPath); statErr == nil {
			return nil, operr.Conflictf("snapshot %d already has a local copy at %s", id, record.LocalPath)
		}
	}
	if _, statErr := os.Stat(localPath); statErr == nil {
		// Refuse rather than overwrite: the file here may be the very archive
		// someone is about to restore from.
		return nil, operr.Conflictf("a file already exists at %s; move it aside first", localPath)
	}

	s3Client, err := s3service.NewClient(ctx, settings)
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}

	key, err := parseRemoteS3Location(record.RemotePath, settings.Bucket)
	if err != nil {
		return nil, operr.Invalidf("snapshot %d has an unusable remote location: %v", id, err)
	}

	size, err := streamToLocalFileAtomic(ctx, s3Client, s3Client.RelativeKey(key), localPath)
	if err != nil {
		logOffsiteFailure(deps, settings, "snapshot_download", record.Filename, err)
		return nil, fmt.Errorf("download snapshot: %w", err)
	}

	if err := deps.Store.Snapshot.SetLocalLocation(id, localPath); err != nil {
		return nil, err
	}

	if err := deps.Store.Offsite.AddLog(&offsite.OffsiteLog{
		Event:             "snapshot_download",
		OffsiteSettingsID: settings.ID,
		Object:            fmt.Sprintf("%s -> %s", record.RemotePath, localPath),
		Success:           true,
		CreatedAt:         rfc3339time.Now(),
	}); err != nil {
		// The file is on disk and the location is recorded; a missing log line
		// must not fail the download.
		log.Printf("WARNING: snapshot %d downloaded but the offsite log row failed: %v", id, err)
	}

	return &DownloadSnapshotResult{
		ID:        id,
		Filename:  record.Filename,
		Size:      size,
		LocalPath: localPath,
	}, nil
}

// RemoveSnapshotLocal deletes the local archive, keeping any offsite copy.
//
// When there is no offsite copy the RECORD is removed too, because
// snapshot_history CHECKs that a row describes at least one location — the same
// semantics RemoveLocalCopy implements for backups.
func RemoveSnapshotLocal(ctx context.Context, deps *Dependencies, id int) error {
	record, err := deps.Store.Snapshot.Get(id)
	if err != nil {
		return err
	}
	if record == nil {
		return operr.NotFoundf("snapshot %d not found", id)
	}
	if record.LocalPath == "" {
		return operr.Invalidf("snapshot %d has no local copy", id)
	}

	if err := os.Remove(record.LocalPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete %s: %w", record.LocalPath, err)
	}
	if record.RemotePath == "" {
		return deps.Store.Snapshot.Delete(id)
	}
	return deps.Store.Snapshot.ClearLocalLocation(id)
}

// RemoveSnapshotRemote deletes the offsite copy, keeping any local one.
func RemoveSnapshotRemote(ctx context.Context, deps *Dependencies, id int) error {
	settings, err := GetActiveOffsiteSettingsDecrypted(deps)
	if err != nil {
		return fmt.Errorf("get offsite settings: %w", err)
	}
	if settings == nil {
		return operr.Invalidf("offsite backup not configured")
	}

	record, err := deps.Store.Snapshot.Get(id)
	if err != nil {
		return err
	}
	if record == nil {
		return operr.NotFoundf("snapshot %d not found", id)
	}
	if record.RemotePath == "" {
		return operr.Invalidf("snapshot %d has no remote copy", id)
	}

	bucket, key, err := parseS3Location(record.RemotePath)
	if err != nil {
		return operr.Invalidf("snapshot %d has an unusable remote location: %v", id, err)
	}
	if bucket != settings.Bucket {
		return operr.Invalidf("snapshot %d lives in bucket %q but offsite is configured for %q; refusing to delete from a bucket ODDK no longer manages",
			id, bucket, settings.Bucket)
	}

	s3Client, err := s3service.NewClient(ctx, settings)
	if err != nil {
		return fmt.Errorf("create S3 client: %w", err)
	}
	if err := s3Client.DeleteFile(ctx, s3Client.RelativeKey(key)); err != nil {
		return fmt.Errorf("delete remote snapshot: %w", err)
	}

	if record.LocalPath == "" {
		return deps.Store.Snapshot.Delete(id)
	}
	return deps.Store.Snapshot.ClearRemoteLocation(id)
}
