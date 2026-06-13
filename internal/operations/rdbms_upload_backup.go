package operations

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hypersequent/oddk/internal/rfc3339time"
	s3service "github.com/hypersequent/oddk/internal/services/s3"
	"github.com/hypersequent/oddk/internal/store/offsite"
)

// UploadBackupParams contains parameters for uploading a backup to S3
type UploadBackupParams struct {
	InstanceName string
	BackupID     int
}

// UploadBackupResult contains the result of uploading a backup
type UploadBackupResult struct {
	Message  string
	Location string
	Size     int64
}

// UploadBackup uploads a backup to S3
func UploadBackup(ctx context.Context, deps *Dependencies, params UploadBackupParams) (*UploadBackupResult, error) {
	settings, err := GetActiveOffsiteSettingsDecrypted(deps)
	if err != nil {
		return nil, fmt.Errorf("get offsite settings: %w", err)
	}
	if settings == nil {
		return nil, fmt.Errorf("offsite backup not configured")
	}

	backupRecord, err := deps.Store.Backup.GetBackupByID(params.BackupID)
	if err != nil {
		return nil, fmt.Errorf("get backup: %w", err)
	}
	if backupRecord == nil {
		return nil, fmt.Errorf("backup not found: %d", params.BackupID)
	}

	// We already have the backup record from the store
	backup := backupRecord

	// Check if backup belongs to the specified instance
	if backup.InstanceName != params.InstanceName {
		return nil, fmt.Errorf("backup %d does not belong to instance %s", params.BackupID, params.InstanceName)
	}

	// Check if already uploaded
	if backup.RemotePath != "" {
		return nil, fmt.Errorf("backup already uploaded to S3: %s", backup.RemotePath)
	}

	// Check if local file exists
	if backup.LocalPath == "" {
		return nil, fmt.Errorf("backup has no local file to upload")
	}

	backupPath := backup.LocalPath
	if !filepath.IsAbs(backupPath) {
		return nil, fmt.Errorf("backup location is not an absolute path: %s", backupPath)
	}

	localFile, err := os.Open(backupPath) // #nosec G304 - path is validated from database
	if err != nil {
		return nil, fmt.Errorf("open backup file: %w", err)
	}
	defer func() { _ = localFile.Close() }()

	fileInfo, err := localFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat backup file: %w", err)
	}
	localSize := fileInfo.Size()

	s3Client, err := s3service.NewClient(ctx, settings)
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}

	// Generate S3 key (without bucket path prefix, as the client adds it automatically)
	// Pattern: instance_name/YYYY-MM-DD/backup_filename
	backupFilename := filepath.Base(backup.LocalPath)
	now := time.Now()
	s3Key := fmt.Sprintf("%s/%s/%s",
		backup.InstanceName,
		now.Format("2006-01-02"),
		backupFilename,
	)

	// Check if file already exists in S3
	exists, err := s3Client.FileExists(ctx, s3Key)
	if err != nil {
		return nil, fmt.Errorf("check S3 file existence: %w", err)
	}

	if exists {
		// File exists, for simplicity we'll re-upload it
		// In the future, we could add size checking if needed
		if err := s3Client.DeleteFile(ctx, s3Key); err != nil {
			return nil, fmt.Errorf("delete existing S3 file: %w", err)
		}
	}

	// Reset file position for upload
	if _, err := localFile.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("reset file position: %w", err)
	}

	// Upload to S3
	if err := s3Client.UploadFile(ctx, s3Key, localFile); err != nil {
		return nil, fmt.Errorf("upload to S3: %w", err)
	}

	// Verify upload by checking existence
	exists, err = s3Client.FileExists(ctx, s3Key)
	if err != nil {
		return nil, fmt.Errorf("verify upload: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("upload verification failed: file not found in S3")
	}

	// Build full S3 location with bucket path prefix for database storage
	var s3Location string
	if s3Client.GetBucketPath() != "" {
		s3Location = fmt.Sprintf("s3://%s/%s%s", settings.Bucket, s3Client.GetBucketPath(), s3Key)
	} else {
		s3Location = fmt.Sprintf("s3://%s/%s", settings.Bucket, s3Key)
	}

	if err := deps.Store.Backup.UpdateRemoteLocation(params.BackupID, s3Location); err != nil {
		return nil, fmt.Errorf("update backup remote location: %w", err)
	}

	logEntry := &offsite.OffsiteLog{
		Event:             "backup_upload",
		OffsiteSettingsID: settings.ID,
		Object:            fmt.Sprintf("%s:%s -> s3://%s/%s", backup.InstanceName, backupFilename, settings.Bucket, s3Key),
		Success:           true,
		ErrorDetails:      nil,
		CreatedAt:         rfc3339time.Now(),
	}
	if err := deps.Store.Offsite.AddLog(logEntry); err != nil {
		// Don't fail the operation if logging fails
		fmt.Printf("Warning: failed to log upload: %v\n", err)
	}

	return &UploadBackupResult{
		Message:  "Backup uploaded successfully",
		Location: s3Location,
		Size:     localSize,
	}, nil
}
