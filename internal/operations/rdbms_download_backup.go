package operations

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	s3service "github.com/hypersequent/oddk/internal/services/s3"
	"github.com/hypersequent/oddk/internal/store/backup"
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

	// The stored key includes the configured bucket path; the client re-adds it.
	written, err := streamToLocalFile(ctx, s3Client, s3Client.RelativeKey(s3Key), localPath)
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

// parseRemoteS3Location extracts the object key from an s3://bucket/key
// location and verifies the bucket matches the active offsite configuration.
func parseRemoteS3Location(s3Location, configuredBucket string) (string, error) {
	if !strings.HasPrefix(s3Location, "s3://") {
		return "", fmt.Errorf("invalid S3 location format: %s", s3Location)
	}

	parts := strings.SplitN(strings.TrimPrefix(s3Location, "s3://"), "/", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid S3 location format: %s", s3Location)
	}

	if parts[0] != configuredBucket {
		return "", fmt.Errorf("backup S3 bucket (%s) doesn't match current configuration (%s)",
			parts[0], configuredBucket)
	}
	return parts[1], nil
}

// streamToLocalFile streams the S3 object to localPath (the client verifies
// the byte count against the response's ContentLength). The partial file is
// removed on any failure.
func streamToLocalFile(ctx context.Context, s3Client *s3service.Client, key, localPath string) (int64, error) {
	localFile, err := os.Create(localPath) // #nosec G304 - path is constructed from safe components
	if err != nil {
		return 0, fmt.Errorf("create local file: %w", err)
	}
	defer func() {
		_ = localFile.Close()
	}()

	written, err := s3Client.DownloadFileTo(ctx, key, localFile)
	if err != nil {
		_ = os.Remove(localPath)
		return 0, err
	}
	return written, nil
}
