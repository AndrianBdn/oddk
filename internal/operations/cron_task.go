package operations

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hypersequent/oddk/internal/services/s3"
)

type CronTaskOp struct {
	deps         *Dependencies
	instanceName string
	cronLogID    int
	backupID     int // Store the created backup ID for upload
}

func NewCronTaskOp(deps *Dependencies, instanceName string) *CronTaskOp {
	return &CronTaskOp{
		deps:         deps,
		instanceName: instanceName,
	}
}

func (op *CronTaskOp) Name() string {
	return fmt.Sprintf("CronTask-%s", op.instanceName)
}

func (op *CronTaskOp) Type() OpType {
	return OpTypeWrite
}

func (op *CronTaskOp) Execute(ctx context.Context) error {
	cronLog, err := op.deps.Store.Cron.CreateLog(op.instanceName)
	if err != nil {
		return fmt.Errorf("creating cron log: %w", err)
	}

	op.cronLogID = cronLog.ID
	log.Printf("Starting cron task for instance %s (log ID: %d)", op.instanceName, op.cronLogID)

	if err := op.runBackup(ctx); err != nil {
		op.updateCronLog("backup_status", "fail")
		op.updateCronLog("backup_error", err.Error())
		op.updateCronLog("backup_finished_at", time.Now().UTC())
		log.Printf("Backup failed for instance %s: %v", op.instanceName, err)
		// If backup fails, we still want to run cleanup
	} else {
		op.updateCronLog("backup_status", "ok")
		op.updateCronLog("backup_finished_at", time.Now().UTC())
		log.Printf("Backup completed for instance %s", op.instanceName)

		if op.backupID > 0 {
			if err := op.runUpload(ctx); err != nil {
				op.updateCronLog("backup_upload_status", "fail")
				op.updateCronLog("backup_upload_error", err.Error())
				op.updateCronLog("backup_upload_finished_at", time.Now().UTC())
				log.Printf("Upload failed for instance %s: %v", op.instanceName, err)
			} else {
				op.updateCronLog("backup_upload_status", "ok")
				op.updateCronLog("backup_upload_finished_at", time.Now().UTC())
				log.Printf("Upload completed for instance %s", op.instanceName)
			}
		}
	}

	// Retry uploads for older backups that never made it offsite (their upload
	// failed on a previous run). Runs before local cleanup so a successfully
	// retried backup can be pruned by local retention in the same pass.
	op.runUploadRetries(ctx)

	if err := op.runLocalCleanup(ctx); err != nil {
		op.updateCronLog("backup_cleanup_status", "fail")
		op.updateCronLog("backup_cleanup_error", err.Error())
		op.updateCronLog("backup_cleanup_finished_at", time.Now().UTC())
		log.Printf("Local cleanup failed for instance %s: %v", op.instanceName, err)
	} else {
		op.updateCronLog("backup_cleanup_status", "ok")
		op.updateCronLog("backup_cleanup_finished_at", time.Now().UTC())
		log.Printf("Local cleanup completed for instance %s", op.instanceName)
	}

	if err := op.runRemoteCleanup(ctx); err != nil {
		op.updateCronLog("backup_remote_cleanup_status", "fail")
		op.updateCronLog("backup_remote_cleanup_error", err.Error())
		op.updateCronLog("backup_remote_cleanup_finished_at", time.Now().UTC())
		log.Printf("Remote cleanup failed for instance %s: %v", op.instanceName, err)
	} else {
		op.updateCronLog("backup_remote_cleanup_status", "ok")
		op.updateCronLog("backup_remote_cleanup_finished_at", time.Now().UTC())
		log.Printf("Remote cleanup completed for instance %s", op.instanceName)
	}

	// Mark cron log as completed
	if err := op.deps.Store.Cron.CompleteLog(op.cronLogID); err != nil {
		log.Printf("Error completing cron log for instance %s: %v", op.instanceName, err)
	}

	return nil
}

func (op *CronTaskOp) runBackup(ctx context.Context) error {
	// Check if instance exists and is running
	instance, err := op.deps.Store.Instances.Get(op.instanceName)
	if err != nil {
		return fmt.Errorf("getting instance: %w", err)
	}
	if instance == nil {
		return fmt.Errorf("instance '%s' not found", op.instanceName)
	}

	if instance.Status != "running" {
		return fmt.Errorf("instance '%s' is not running (status: %s)", op.instanceName, instance.Status)
	}

	params := &BackupRDBMSParams{
		Name:      op.instanceName,
		BackupDir: op.deps.BackupDir,
		Comment:   "Automatic backup via cron",
	}

	result, err := BackupRDBMS(ctx, op.deps, params)
	if err != nil {
		return err
	}

	// Store the backup ID for potential upload
	if result != nil && result.BackupID > 0 {
		op.backupID = result.BackupID
	}

	return nil
}

func (op *CronTaskOp) runUpload(ctx context.Context) error {
	// Check if offsite is configured
	offsiteConfig, err := op.deps.Store.Offsite.GetActive()
	if err != nil || offsiteConfig == nil {
		log.Printf("Skipping upload for instance %s: offsite not configured", op.instanceName)
		return nil // Not an error, just skip upload
	}

	// Upload the backup
	uploadParams := UploadBackupParams{
		InstanceName: op.instanceName,
		BackupID:     op.backupID,
	}

	result, err := UploadBackup(ctx, op.deps, uploadParams)
	if err != nil {
		return fmt.Errorf("uploading backup: %w", err)
	}

	log.Printf("Uploaded backup %d for instance %s to %s (size: %d bytes)",
		op.backupID, op.instanceName, result.Location, result.Size)
	return nil
}

// runUploadRetries uploads any completed backup that has a local copy but no
// remote copy while offsite is configured. Together with the local-cleanup
// safeguard (which refuses to age out local-only backups), this guarantees a
// backup whose upload failed eventually reaches S3 instead of staying
// local-only forever. Tonight's backup is skipped: runUpload just handled it,
// and if that upload failed, an immediate retry would almost certainly fail
// the same way — the next cron run picks it up. Failures are logged per
// backup and never fail the cron task.
func (op *CronTaskOp) runUploadRetries(ctx context.Context) {
	offsiteConfig, err := op.deps.Store.Offsite.GetActive()
	if err != nil || offsiteConfig == nil {
		return
	}

	backups, err := op.deps.Store.Backup.ListBackups(op.instanceName)
	if err != nil {
		log.Printf("Warning: skipping upload retries for instance %s: listing backups: %v", op.instanceName, err)
		return
	}

	retried, failed := 0, 0
	for _, backup := range backups {
		if backup.ID == op.backupID || backup.Status != "completed" {
			continue
		}
		if !backup.LocalLocation.Valid || backup.RemoteLocation.Valid {
			continue
		}
		result, err := UploadBackup(ctx, op.deps, UploadBackupParams{
			InstanceName: op.instanceName,
			BackupID:     backup.ID,
		})
		if err != nil {
			failed++
			log.Printf("Warning: retry upload of backup %d for instance %s failed: %v", backup.ID, op.instanceName, err)
			continue
		}
		retried++
		log.Printf("Retried upload of backup %d for instance %s to %s (size: %d bytes)",
			backup.ID, op.instanceName, result.Location, result.Size)
	}

	if retried > 0 || failed > 0 {
		log.Printf("Upload retries for instance %s: %d uploaded, %d failed", op.instanceName, retried, failed)
	}
}

func (op *CronTaskOp) runLocalCleanup(ctx context.Context) error {
	plan, err := op.deps.Store.Cron.GetPlan(op.instanceName)
	if err != nil {
		// Plan might have been deleted - skip cleanup gracefully
		log.Printf("Warning: unable to get cron plan for local cleanup of %s: %v", op.instanceName, err)
		return nil
	}

	backups, err := op.deps.Store.Backup.ListBackups(op.instanceName)
	if err != nil {
		return fmt.Errorf("listing backups: %w", err)
	}

	// When offsite is configured, the local copy may be a backup's ONLY copy if
	// its upload failed (uploads are not retried). Never age out such a copy —
	// otherwise a single failed upload night silently loses that backup
	// entirely. Without offsite, local-only retention applies as configured.
	offsiteConfigured := false
	if cfg, err := op.deps.Store.Offsite.GetActive(); err == nil && cfg != nil {
		offsiteConfigured = true
	}

	now := time.Now()
	localCutoff := now.AddDate(0, 0, -plan.CleanupLocalDays)

	localDeleted := 0

	for _, backup := range backups {
		backupTime := backup.Timestamp.Time

		if backup.LocalLocation.Valid && backupTime.Before(localCutoff) {
			if offsiteConfigured && !backup.RemoteLocation.Valid {
				log.Printf("Warning: keeping local backup %d for instance %s past retention: offsite is configured but this backup has no remote copy (upload it or remove it manually)",
					backup.ID, op.instanceName)
				continue
			}
			if err := op.deps.Store.Backup.RemoveLocalCopy(backup.ID, op.instanceName); err != nil {
				log.Printf("Warning: failed to remove local copy of backup %d: %v", backup.ID, err)
			} else {
				localDeleted++
			}
		}
	}

	if localDeleted > 0 {
		log.Printf("Local cleanup for instance %s: removed %d backup(s) older than %d days",
			op.instanceName, localDeleted, plan.CleanupLocalDays)
	}

	return nil
}

func (op *CronTaskOp) runRemoteCleanup(ctx context.Context) error {
	// Check if offsite is configured. Decrypt the secret here (via the shared
	// helper) so the S3 client gets the plaintext key — passing the stored
	// ciphertext made every real-S3 delete fail with a signature error, so aged
	// remote backups were never cleaned up.
	offsiteConfig, err := GetActiveOffsiteSettingsDecrypted(op.deps)
	if err != nil || offsiteConfig == nil {
		log.Printf("Skipping remote cleanup for instance %s: offsite not configured", op.instanceName)
		return nil // Not an error, just skip remote cleanup
	}

	plan, err := op.deps.Store.Cron.GetPlan(op.instanceName)
	if err != nil {
		// Plan might have been deleted - skip cleanup gracefully
		log.Printf("Warning: unable to get cron plan for remote cleanup of %s: %v", op.instanceName, err)
		return nil
	}

	backups, err := op.deps.Store.Backup.ListBackups(op.instanceName)
	if err != nil {
		return fmt.Errorf("listing backups: %w", err)
	}

	now := time.Now()
	remoteCutoff := now.AddDate(0, 0, -plan.CleanupRemoteDays)

	remoteDeleted := 0

	s3Client, err := s3.NewClient(ctx, offsiteConfig)
	if err != nil {
		return fmt.Errorf("creating S3 client: %w", err)
	}

	for _, backup := range backups {
		backupTime := backup.Timestamp.Time

		if backup.RemoteLocation.Valid && backupTime.Before(remoteCutoff) {
			remotePath := backup.RemoteLocation.String
			if s3Path, ok := strings.CutPrefix(remotePath, "s3://"); ok {
				pathParts := strings.SplitN(s3Path, "/", 2)
				if len(pathParts) == 2 {
					bucketName := pathParts[0]
					fullKey := pathParts[1] // e.g., "cron-cleanup-test/instance/2024-01-01/backup.tar.zst"

					if bucketName != offsiteConfig.Bucket {
						log.Printf("Warning: skipping deletion of backup %d - bucket mismatch (stored: %s, configured: %s)",
							backup.ID, bucketName, offsiteConfig.Bucket)
						continue
					}

					// The stored key includes the configured bucket path; the
					// client re-adds it, so strip it here.
					keyWithoutPrefix := s3Client.RelativeKey(fullKey)

					if err := s3Client.DeleteFile(ctx, keyWithoutPrefix); err != nil {
						log.Printf("Warning: failed to delete remote backup %d from S3: %v", backup.ID, err)
					} else {
						// Clear remote location in database after successful S3 deletion
						if err := op.deps.Store.Backup.RemoveRemoteCopy(backup.ID, op.instanceName); err != nil {
							log.Printf("Warning: failed to clear remote location for backup %d: %v", backup.ID, err)
						} else {
							remoteDeleted++
						}
					}
				}
			}
		}
	}

	if remoteDeleted > 0 {
		log.Printf("Remote cleanup for instance %s: removed %d backup(s) older than %d days",
			op.instanceName, remoteDeleted, plan.CleanupRemoteDays)
	}

	return nil
}

func (op *CronTaskOp) updateCronLog(field string, value any) {
	if err := op.deps.Store.Cron.UpdateLog(op.cronLogID, map[string]any{field: value}); err != nil {
		log.Printf("Error updating cron log field %s for instance %s: %v", field, op.instanceName, err)
	}
}
