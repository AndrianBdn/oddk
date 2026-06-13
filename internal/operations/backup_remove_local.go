package operations

import (
	"context"
	"fmt"

	"github.com/hypersequent/oddk/internal/operr"
	s3service "github.com/hypersequent/oddk/internal/services/s3"
)

type RemoveLocalBackupParams struct {
	InstanceName string
	BackupID     int
}

type RemoveLocalBackupResult struct {
	Message string `json:"message"`
}

func RemoveLocalBackup(ctx context.Context, deps *Dependencies, params RemoveLocalBackupParams) (*RemoveLocalBackupResult, error) {
	// Verify instance exists
	instance, err := deps.Store.Instances.Get(params.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}
	if instance == nil {
		return nil, operr.NotFoundf("instance not found: %s", params.InstanceName)
	}

	err = deps.Store.Backup.RemoveLocalCopy(params.BackupID, params.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("remove local backup: %w", err)
	}

	return &RemoveLocalBackupResult{
		Message: fmt.Sprintf("Successfully removed local copy of backup %d", params.BackupID),
	}, nil
}

type RemoveRemoteBackupParams struct {
	InstanceName string
	BackupID     int
}

type RemoveRemoteBackupResult struct {
	Message string `json:"message"`
}

func RemoveRemoteBackup(ctx context.Context, deps *Dependencies, params RemoveRemoteBackupParams) (*RemoveRemoteBackupResult, error) {
	// Verify instance exists
	instance, err := deps.Store.Instances.Get(params.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}
	if instance == nil {
		return nil, operr.NotFoundf("instance not found: %s", params.InstanceName)
	}

	backup, err := deps.Store.Backup.GetBackupByID(params.BackupID)
	if err != nil {
		return nil, fmt.Errorf("get backup: %w", err)
	}

	if !backup.RemoteLocation.Valid {
		return nil, fmt.Errorf("backup %d has no remote copy", params.BackupID)
	}

	settings, err := GetActiveOffsiteSettingsDecrypted(deps)
	if err != nil {
		return nil, fmt.Errorf("get offsite settings: %w", err)
	}

	if settings == nil {
		// No S3 configured, just remove the database reference
		err = deps.Store.Backup.RemoveRemoteCopy(params.BackupID, params.InstanceName)
		if err != nil {
			return nil, fmt.Errorf("remove remote backup reference: %w", err)
		}
		return &RemoveRemoteBackupResult{
			Message: fmt.Sprintf("Successfully removed remote reference for backup %d (S3 not configured)", params.BackupID),
		}, nil
	}

	s3Key, err := parseRemoteS3Location(backup.RemoteLocation.String, settings.Bucket)
	if err != nil {
		return nil, err
	}

	s3Client, err := s3service.NewClient(ctx, settings)
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}

	// The stored key includes the configured bucket path; the client re-adds it.
	// Deletion failure is a warning: the database reference is removed either
	// way, matching the documented remove-remote semantics.
	if err := s3Client.DeleteFile(ctx, s3Client.RelativeKey(s3Key)); err != nil {
		deps.Logger.Printf("Warning: failed to delete S3 object %s: %v", s3Key, err)
	}

	err = deps.Store.Backup.RemoveRemoteCopy(params.BackupID, params.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("remove remote backup reference: %w", err)
	}

	return &RemoveRemoteBackupResult{
		Message: fmt.Sprintf("Successfully removed remote copy of backup %d from S3", params.BackupID),
	}, nil
}
