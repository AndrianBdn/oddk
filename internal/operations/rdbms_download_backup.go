package operations

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrianbdn/oddk/internal/compression"
	s3service "github.com/andrianbdn/oddk/internal/services/s3"
	"github.com/andrianbdn/oddk/internal/store/backup"
)

// DownloadBackupParams contains parameters for downloading a backup from S3
type DownloadBackupParams struct {
	InstanceName string
	BackupID     int
}

// DownloadBackupResult contains the result of downloading a backup
type DownloadBackupResult struct {
	Message  string `json:"message"`
	Location string `json:"location"`
	Size     int64  `json:"size"`
}

// DownloadBackup downloads a backup from S3 to local storage
func DownloadBackup(ctx context.Context, deps *Dependencies, params DownloadBackupParams) (*DownloadBackupResult, error) {
	backup, err := validateDownloadableBackup(deps, params)
	if err != nil {
		return nil, err
	}

	settings, err := GetActiveOffsiteSettingsDecrypted(deps)
	if err != nil {
		return nil, fmt.Errorf("get offsite settings: %w", err)
	}
	if settings == nil {
		return nil, fmt.Errorf("offsite backup not configured")
	}

	s3Key, err := parseRemoteS3Location(backup.RemoteLocation.String, settings.Bucket)
	if err != nil {
		return nil, err
	}

	s3Client, err := s3service.NewClient(ctx, settings)
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}

	// Local path: backupDir/<filename>, filename = last component of the S3 key
	// (backup-<instance>-<timestamp>-<id>.tar.zst)
	keyParts := strings.Split(s3Key, "/")
	localPath := filepath.Join(deps.BackupDir, keyParts[len(keyParts)-1])

	// Atomic: the stream lands in a temp file, is verified, and only then takes
	// the real archive name. Writing straight to localPath left a truncated file
	// AT a genuine backup name on a crash, and the daemon's startup sweep only
	// REPORTS unreferenced archives — it never deletes them, precisely because
	// one might be a file an operator parked there for 'backup restore --file'.
	// The stored key includes the configured bucket path; the client re-adds it.
	written, err := streamToLocalFileAtomic(ctx, s3Client, s3Client.RelativeKey(s3Key), localPath)
	if err != nil {
		return nil, err
	}

	if err := deps.Store.Backup.UpdateLocalLocation(params.BackupID, localPath); err != nil {
		// Clean up downloaded file since we couldn't update database
		_ = os.Remove(localPath)
		return nil, fmt.Errorf("update backup local location: %w", err)
	}

	return &DownloadBackupResult{
		Message:  fmt.Sprintf("Successfully downloaded backup %d from S3", params.BackupID),
		Location: localPath,
		Size:     written,
	}, nil
}

// validateDownloadableBackup loads the backup record and checks it can be
// downloaded: it belongs to the instance, has a remote copy, and does not
// already have a local file.
func validateDownloadableBackup(deps *Dependencies, params DownloadBackupParams) (*backup.BackupRecord, error) {
	record, err := deps.Store.Backup.GetBackupByID(params.BackupID)
	if err != nil {
		return nil, fmt.Errorf("get backup: %w", err)
	}
	if record == nil {
		return nil, fmt.Errorf("backup not found: %d", params.BackupID)
	}

	if record.InstanceName != params.InstanceName {
		return nil, fmt.Errorf("backup %d does not belong to instance %s", params.BackupID, params.InstanceName)
	}

	if !record.RemoteLocation.Valid || record.RemoteLocation.String == "" {
		return nil, fmt.Errorf("backup %d has no remote copy to download", params.BackupID)
	}

	if record.LocalLocation.Valid && record.LocalLocation.String != "" {
		localPath := record.LocalLocation.String
		if !filepath.IsAbs(localPath) {
			localPath = filepath.Join(deps.BackupDir, localPath)
		}
		if _, err := os.Stat(localPath); err == nil {
			return nil, fmt.Errorf("backup %d already has a local copy at %s", params.BackupID, localPath)
		}
	}

	return record, nil
}

// streamToLocalFile streams the S3 object to localPath (the client verifies the
// byte count against the response's ContentLength), fsyncs it, and checks the
// Close. The partial file is removed on any failure.
//
// The Close error used to be discarded by a deferred call that ran AFTER the
// function had already returned success — so it was not merely ignored, it was
// unreachable. A writeback error surfaced at close (the usual way ENOSPC and
// network-filesystem failures are reported) produced a short file that every
// caller then treated as a complete download. The fsync is what makes the
// subsequent rename meaningful: renaming a file whose data is still only in the
// page cache can leave a zero-length archive at the final name after a power
// loss.
func streamToLocalFile(ctx context.Context, s3Client *s3service.Client, key, localPath string) (int64, error) {
	// 0600, not os.Create's 0666&^umask (0644 under the systemd unit): an archive
	// holds every database's contents and the role password hashes, and is not
	// encrypted by the master key. Archives WRITTEN here are already 0600
	// (writeVerifiedArchive chmods the temp file for exactly this reason), so a
	// downloaded one landing world-readable in the same directory was an
	// unintended asymmetry — and a restore is precisely when a host pulls the
	// whole deployment's data down.
	localFile, err := os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 - path is constructed from safe components
	if err != nil {
		return 0, fmt.Errorf("create local file: %w", err)
	}

	written, err := s3Client.DownloadFileTo(ctx, key, localFile)
	if err == nil {
		err = localFile.Sync()
	}
	if closeErr := localFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(localPath)
		return 0, err
	}
	return written, nil
}

// verifyDownloadedArchive proves a freshly downloaded archive is intact before
// anything is allowed to depend on it — before a catalogue row claims a local
// copy, before a provenance sidecar vouches for it, before it is renamed to a
// name that reads as a real archive.
//
// S3 integrity checking here was ContentLength plus an ETag sidecar, which is
// provenance, not integrity: neither says the BYTES ON THIS DISK are the bytes
// that were uploaded. VerifyTarZstd drains the zstd frame, so it catches the
// single-bit corruption that a length check cannot see (see
// internal/compression). Restores verify too, since v0.1.70 — but discovering
// at restore time that the archive you downloaded a week ago is corrupt is
// discovering it at the worst possible moment.
func verifyDownloadedArchive(ctx context.Context, path string) error {
	if _, err := compression.NewCompressor().VerifyTarZstd(ctx, path); err != nil {
		return fmt.Errorf("the downloaded archive failed verification and was discarded (%w); "+
			"re-run the download, and if it fails again the copy in the bucket is damaged", err)
	}
	return nil
}
