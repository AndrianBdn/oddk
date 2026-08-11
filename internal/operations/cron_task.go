package operations

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/andrianbdn/oddk/internal/services/s3"
)

type CronTaskOp struct {
	deps         *Dependencies
	instanceName string
	cronLogID    int
	backupID     int // Store the created backup ID for upload
	reporter     *cronRunReporter
}

// phase records one phase's outcome on the cron log, and on the run reporter
// that decides whether the operator hears about this run. It replaces four
// copies of the same three updateCronLog calls.
func (op *CronTaskOp) phase(name, status string, cause error) {
	op.updateCronLog(name+"_status", status)
	op.updateCronLog(name+"_finished_at", time.Now().UTC())
	if cause != nil {
		op.updateCronLog(name+"_error", cause.Error())
	}
	if op.reporter != nil {
		op.reporter.record(name, status, cause)
	}
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
	// Sample the previous run BEFORE creating this run's row, or the "previous
	// run" the reporter finds is this one.
	op.reporter = newCronRunReporter(op.deps, op.instanceName,
		fmt.Sprintf("instance %q", op.instanceName), "Backup")

	cronLog, err := op.deps.Store.Cron.CreateLog(op.instanceName)
	if err != nil {
		return fmt.Errorf("creating cron log: %w", err)
	}

	op.cronLogID = cronLog.ID
	log.Printf("Starting cron task for instance %s (log ID: %d)", op.instanceName, op.cronLogID)

	if err := op.runBackup(ctx); err != nil {
		op.phase("backup", "fail", err)
		log.Printf("Backup failed for instance %s: %v", op.instanceName, err)
		// If backup fails, we still want to run cleanup
	} else {
		op.phase("backup", "ok", nil)
		log.Printf("Backup completed for instance %s", op.instanceName)

		if op.backupID > 0 {
			if err := op.runUpload(ctx); err != nil {
				op.phase("backup_upload", "fail", err)
				log.Printf("Upload failed for instance %s: %v", op.instanceName, err)
			} else {
				op.phase("backup_upload", "ok", nil)
				log.Printf("Upload completed for instance %s", op.instanceName)
			}
		}
	}

	// Retry uploads for older backups that never made it offsite (their upload
	// failed on a previous run). Runs before local cleanup so a successfully
	// retried backup can be pruned by local retention in the same pass.
	op.runUploadRetries(ctx)

	if err := op.runLocalCleanup(); err != nil {
		op.phase("backup_cleanup", "fail", err)
		log.Printf("Local cleanup failed for instance %s: %v", op.instanceName, err)
	} else {
		op.phase("backup_cleanup", "ok", nil)
		log.Printf("Local cleanup completed for instance %s", op.instanceName)
	}

	if err := op.runRemoteCleanup(ctx); err != nil {
		op.phase("backup_remote_cleanup", "fail", err)
		log.Printf("Remote cleanup failed for instance %s: %v", op.instanceName, err)
	} else {
		op.phase("backup_remote_cleanup", "ok", nil)
		log.Printf("Remote cleanup completed for instance %s", op.instanceName)
	}

	// Mark cron log as completed
	if err := op.deps.Store.Cron.CompleteLog(op.cronLogID); err != nil {
		log.Printf("Error completing cron log for instance %s: %v", op.instanceName, err)
	}

	// Last, and never fatal: a notification problem must not turn a successful
	// backup into a failed cron task.
	op.reporter.finish(ctx)

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

// minRetainedBackups is the floor age-based local retention may never cross,
// for the same reason as minRetainedSnapshots: retention runs even on a night
// the capture failed, so an age-only rule turns a long-failing backup job into
// total data loss. A stale backup beats none.
const minRetainedBackups = 2

func (op *CronTaskOp) runLocalCleanup() error {
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
	// its upload failed. Never age out such a copy — otherwise a single failed
	// upload night silently loses that backup entirely. Without offsite,
	// local-only retention applies as configured.
	//
	// Fail SAFE, not open. An error reading the settings is not the same as
	// "offsite is not configured": treating it as unconfigured would switch off
	// the only-copy safeguard below and let retention delete a local archive
	// whose remote counterpart we simply failed to look up. (This mirrors
	// SnapshotCronTaskOp.runLocalCleanup, where the same reasoning is spelled
	// out — the two paths must not diverge on a data-safety rule.)
	offsiteConfigured := true
	cfg, cfgErr := op.deps.Store.Offsite.GetActive()
	switch {
	case cfgErr != nil:
		log.Printf("Warning: could not read offsite settings during backup retention for %s (%v); assuming offsite IS configured so the only-copy safeguard stays on",
			op.instanceName, cfgErr)
	case cfg == nil:
		offsiteConfigured = false
	}

	now := time.Now()
	localCutoff := now.AddDate(0, 0, -plan.CleanupLocalDays)

	localDeleted := 0

	// ListBackups is newest-first, so protecting the floor is a matter of
	// counting how many still-present local copies we have walked past. Without
	// it, a backup job that has been failing for longer than cleanup_local_days
	// expires EVERY archive and leaves this instance with nothing to restore
	// from, precisely when it is least able to make a new one — retention runs
	// even on a night the capture failed, deliberately. Same floor, and same
	// reason, as minRetainedSnapshots — and, like it, counting SURVIVING copies
	// rather than catalogue rows (see retentionFloor).
	floor := newRetentionFloor(minRetainedBackups)

	for _, backup := range backups {
		if !backup.LocalLocation.Valid {
			continue
		}

		present := localArchivePresent(op.deps.DataDir, backup.LocalLocation.String)
		if !present {
			log.Printf("Warning: backup %d for instance %s is catalogued with a local copy at %s but the file is not there; it does not count toward the newest-%d floor",
				backup.ID, op.instanceName, backup.LocalLocation.String, minRetainedBackups)
		}

		if floor.protects(present) {
			if backup.Timestamp.Before(localCutoff) {
				log.Printf("Keeping local backup %d for instance %s past retention: it is one of the newest %d, and expiring every archive would leave nothing to restore from",
					backup.ID, op.instanceName, minRetainedBackups)
			}
			continue
		}

		if !backup.Timestamp.Before(localCutoff) {
			continue
		}

		// The only-copy safeguard protects a real copy; a record with no file
		// behind it protects nothing and would otherwise be stranded forever.
		if present && offsiteConfigured && !backup.RemoteLocation.Valid {
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

	// Same floor as local retention, and it matters more offsite: the remote
	// copy is the one that survives the host. ListBackups is newest-first.
	// Existence is checked only while the floor is still filling, so this costs
	// a couple of HeadObject calls per run rather than one per backup.
	floor := newRetentionFloor(minRetainedBackups)

	for _, backup := range backups {
		backupTime := backup.Timestamp.Time

		if !backup.RemoteLocation.Valid {
			continue
		}

		if !floor.full() {
			present := remoteArchivePresent(ctx, s3Client, offsiteConfig.Bucket, backup.RemoteLocation.String)
			if !present {
				log.Printf("Warning: backup %d for instance %s is catalogued with a remote copy at %s but the object is not in the bucket; it does not count toward the newest-%d floor",
					backup.ID, op.instanceName, backup.RemoteLocation.String, minRetainedBackups)
			}
			if floor.protects(present) {
				if backupTime.Before(remoteCutoff) {
					log.Printf("Keeping offsite backup %d for instance %s past retention: it is one of the newest %d",
						backup.ID, op.instanceName, minRetainedBackups)
				}
				continue
			}
		}

		if backupTime.Before(remoteCutoff) {
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
