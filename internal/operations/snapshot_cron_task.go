package operations

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	s3service "github.com/andrianbdn/oddk/internal/services/s3"
	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
)

// SnapshotCronInstance is the identity a scheduled snapshot uses in cron_logs
// and in the task tracker's dedup queue.
//
// Snapshots cover the whole deployment, so they have no instance name — but the
// tracker dedups by name and cron_logs.instance_name is NOT NULL. '*' cannot
// appear in a real instance name (letters, digits, '-' and '_' only, per
// util.ValidateInstanceName), so this can never collide with one.
const SnapshotCronInstance = "*snapshot*"

// minRetainedSnapshots is the floor age-based retention may never cross.
//
// Retention runs even on a night the capture failed — deliberately, so a broken
// snapshot job does not also stop pruning. But with only an age rule, a schedule
// that has been failing for longer than cleanup_local_days expires EVERY archive
// and leaves the deployment with nothing to restore from, precisely when it is
// least able to make a new one. Keeping the newest few regardless of age means
// the worst case is a stale snapshot rather than no snapshot.
const minRetainedSnapshots = 2

// SnapshotCronTaskOp is the scheduled whole-deployment snapshot: capture,
// ship offsite, retry anything that failed to ship, then apply retention.
//
// It reuses cron_logs rather than introducing a parallel table, so snapshots
// inherit the audit trail and the 365-day retention sweep that already exist —
// and so a crashed attempt leaves evidence instead of only a reclaimed staging
// directory. The four phase columns line up: backup -> snapshot, upload,
// cleanup -> local retention, remote_cleanup -> offsite retention.
type SnapshotCronTaskOp struct {
	deps       *Dependencies
	backupDir  string
	cronLogID  int
	snapshotID int
}

func NewSnapshotCronTaskOp(deps *Dependencies, backupDir string) *SnapshotCronTaskOp {
	return &SnapshotCronTaskOp{deps: deps, backupDir: backupDir}
}

func (op *SnapshotCronTaskOp) Name() string { return "SnapshotCronTask" }

func (op *SnapshotCronTaskOp) Type() OpType { return OpTypeWrite }

// Execute runs every phase, recording each and never aborting the chain on a
// phase failure — retention must still run on a night the capture failed, or a
// broken snapshot job would also silently stop pruning.
func (op *SnapshotCronTaskOp) Execute(ctx context.Context) error {
	cronLog, err := op.deps.Store.Cron.CreateLog(SnapshotCronInstance)
	if err != nil {
		return fmt.Errorf("creating snapshot cron log: %w", err)
	}
	op.cronLogID = cronLog.ID
	log.Printf("Starting scheduled snapshot (log ID: %d)", op.cronLogID)

	if err := op.runSnapshot(ctx); err != nil {
		op.phase("backup", "fail", err)
		log.Printf("Scheduled snapshot failed: %v", err)
	} else {
		op.phase("backup", "ok", nil)

		if err := op.runUpload(ctx); err != nil {
			op.phase("backup_upload", "fail", err)
			log.Printf("Snapshot upload failed: %v", err)
		} else {
			op.phase("backup_upload", "ok", nil)
		}
	}

	// Retry uploads BEFORE local cleanup, so a snapshot whose upload previously
	// failed can be shipped and then pruned in the same pass rather than piling
	// up locally forever.
	op.runUploadRetries(ctx)

	if err := op.runLocalCleanup(); err != nil {
		op.phase("backup_cleanup", "fail", err)
	} else {
		op.phase("backup_cleanup", "ok", nil)
	}

	if err := op.runRemoteCleanup(ctx); err != nil {
		op.phase("backup_remote_cleanup", "fail", err)
	} else {
		op.phase("backup_remote_cleanup", "ok", nil)
	}

	if err := op.deps.Store.Cron.CompleteLog(op.cronLogID); err != nil {
		log.Printf("Warning: could not complete snapshot cron log %d: %v", op.cronLogID, err)
	}
	return nil
}

// phase records one phase's outcome on the cron log.
func (op *SnapshotCronTaskOp) phase(name, status string, cause error) {
	op.set(name+"_status", status)
	op.set(name+"_finished_at", time.Now().UTC())
	if cause != nil {
		op.set(name+"_error", cause.Error())
	}
}

func (op *SnapshotCronTaskOp) set(column string, value any) {
	if err := op.deps.Store.Cron.UpdateLog(op.cronLogID, map[string]any{column: value}); err != nil {
		log.Printf("Warning: could not update snapshot cron log %d (%s): %v", op.cronLogID, column, err)
	}
}

func (op *SnapshotCronTaskOp) runSnapshot(ctx context.Context) error {
	result, err := MakeSnapshot(ctx, op.deps, &MakeSnapshotParams{
		BackupDir: op.backupDir,
		Comment:   "scheduled",
	})
	if err != nil {
		return err
	}
	op.snapshotID = result.ID
	log.Printf("Scheduled snapshot created: %s (%d bytes, %d instance(s) with data, %d configuration-only)",
		result.Path, result.Size, result.InstancesWithData, result.ConfigOnly)
	if result.ConfigOnly > 0 {
		// A configuration-only instance restores to an empty cluster, so this
		// must be visible in the operational log, not just on an interactive run.
		log.Printf("WARNING: scheduled snapshot captured %d instance(s) configuration-only; they hold NO database contents",
			result.ConfigOnly)
	}
	return nil
}

func (op *SnapshotCronTaskOp) runUpload(ctx context.Context) error {
	if op.snapshotID == 0 {
		return nil // nothing recorded, nothing to ship
	}
	settings, err := GetActiveOffsiteSettingsDecrypted(op.deps)
	if err != nil {
		return fmt.Errorf("get offsite settings: %w", err)
	}
	if settings == nil {
		return nil // offsite not configured: not a failure
	}
	_, err = UploadSnapshot(ctx, op.deps, op.snapshotID)
	return err
}

// runUploadRetries ships any earlier snapshot that has a local copy but no
// remote one. Never fails the task.
func (op *SnapshotCronTaskOp) runUploadRetries(ctx context.Context) {
	settings, err := GetActiveOffsiteSettingsDecrypted(op.deps)
	if err != nil || settings == nil {
		return
	}
	records, err := op.deps.Store.Snapshot.List()
	if err != nil {
		log.Printf("Warning: could not list snapshots for upload retry: %v", err)
		return
	}

	// Never re-ship something offsite retention has already expired. A snapshot
	// past cleanup_remote_days has had its remote copy deleted ON PURPOSE, and
	// "has a local copy but no remote copy" cannot tell that apart from a failed
	// upload — so without this the pair would fight: retry uploads it, remote
	// cleanup deletes it, every single run, moving the whole archive each time.
	var remoteCutoff time.Time
	if plan, planErr := op.deps.Store.Snapshot.GetPlan(); planErr == nil && plan != nil {
		remoteCutoff = time.Now().AddDate(0, 0, -plan.CleanupRemoteDays)
	}

	for _, rec := range records {
		if rec.ID == op.snapshotID || rec.RemotePath != "" || rec.LocalPath == "" {
			continue
		}
		if rec.Status != "completed" {
			continue
		}
		if !remoteCutoff.IsZero() && rec.CreatedAt.Before(remoteCutoff) {
			continue
		}
		if _, err := UploadSnapshot(ctx, op.deps, rec.ID); err != nil {
			log.Printf("Warning: retry upload of snapshot %d failed: %v", rec.ID, err)
			continue
		}
		log.Printf("Retried upload of snapshot %d succeeded", rec.ID)
	}
}

// runLocalCleanup ages out local snapshot archives.
//
// It carries the same safeguard as backup retention: with offsite configured, a
// local copy with no remote copy is that snapshot's ONLY copy, so it is kept
// past retention and warned about rather than deleted. Without offsite,
// local-only retention applies as configured and the record goes with the file.
func (op *SnapshotCronTaskOp) runLocalCleanup() error {
	plan, err := op.deps.Store.Snapshot.GetPlan()
	if err != nil {
		return err
	}
	if plan == nil {
		return nil // schedule removed mid-run
	}

	// Fail SAFE, not open. An error reading the settings is not the same as
	// "offsite is not configured": treating it as unconfigured would switch off
	// the only-copy safeguard below and let retention delete a local archive
	// whose remote counterpart we simply failed to look up.
	offsiteConfigured := true
	cfg, cfgErr := op.deps.Store.Offsite.GetActive()
	switch {
	case cfgErr != nil:
		log.Printf("Warning: could not read offsite settings during snapshot retention (%v); assuming offsite IS configured so the only-copy safeguard stays on", cfgErr)
	case cfg == nil:
		offsiteConfigured = false
	}

	cutoff := time.Now().AddDate(0, 0, -plan.CleanupLocalDays)
	records, err := op.deps.Store.Snapshot.List()
	if err != nil {
		return err
	}

	// List() is newest-first, so protecting the floor is a matter of counting
	// how many still-present copies we have walked past.
	kept := 0
	deleted := 0
	for _, rec := range records {
		if rec.LocalPath == "" {
			continue
		}
		if kept < minRetainedSnapshots {
			kept++
			if rec.CreatedAt.Before(cutoff) {
				log.Printf("Keeping local snapshot %d past retention: it is one of the newest %d, and expiring every archive would leave nothing to restore from",
					rec.ID, minRetainedSnapshots)
			}
			continue
		}
		if !rec.CreatedAt.Before(cutoff) {
			continue
		}
		if offsiteConfigured && rec.RemotePath == "" {
			// The safeguard exists because "no remote copy" usually means the
			// upload failed and will be retried. For an archive above the
			// PutObject limit that is never true: it can NEVER be uploaded, so
			// holding it forever is not protecting a recoverable copy, it is
			// filling the disk until snapshots stop working entirely. Let normal
			// retention apply — the newest-N floor above still guarantees a
			// local copy survives.
			if rec.Size > maxPutObjectBytes {
				log.Printf("Warning: local snapshot %d is %.1f GiB, above the %d GiB single-PutObject limit, so it can never be uploaded; applying local retention to it rather than keeping it forever",
					rec.ID, float64(rec.Size)/(1024*1024*1024), maxPutObjectBytes/(1024*1024*1024))
			} else {
				log.Printf("Warning: keeping local snapshot %d past retention: offsite is configured but it has no remote copy (upload it or remove it manually)", rec.ID)
				continue
			}
		}
		if err := op.removeLocalSnapshot(rec); err != nil {
			log.Printf("Warning: could not remove local snapshot %d: %v", rec.ID, err)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		log.Printf("Snapshot local cleanup: removed %d archive(s) older than %d days", deleted, plan.CleanupLocalDays)
	}
	return nil
}

// removeLocalSnapshot deletes the archive and then either clears the local
// location or deletes the record entirely.
//
// Deleting the record when there is no remote copy is required, not a choice:
// snapshot_history CHECKs that at least one location is present, so clearing the
// last one would fail the constraint.
func (op *SnapshotCronTaskOp) removeLocalSnapshot(rec *snapshotstore.Record) error {
	if err := os.Remove(rec.LocalPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete %s: %w", rec.LocalPath, err)
	}
	if rec.RemotePath == "" {
		return op.deps.Store.Snapshot.Delete(rec.ID)
	}
	return op.deps.Store.Snapshot.ClearLocalLocation(rec.ID)
}

// runRemoteCleanup ages out offsite snapshot copies.
func (op *SnapshotCronTaskOp) runRemoteCleanup(ctx context.Context) error {
	settings, err := GetActiveOffsiteSettingsDecrypted(op.deps)
	if err != nil {
		return fmt.Errorf("get offsite settings: %w", err)
	}
	if settings == nil {
		return nil
	}
	plan, err := op.deps.Store.Snapshot.GetPlan()
	if err != nil || plan == nil {
		return err
	}

	s3Client, err := s3service.NewClient(ctx, settings)
	if err != nil {
		return fmt.Errorf("create S3 client: %w", err)
	}

	cutoff := time.Now().AddDate(0, 0, -plan.CleanupRemoteDays)
	records, err := op.deps.Store.Snapshot.List()
	if err != nil {
		return err
	}

	kept := 0
	deleted := 0
	for _, rec := range records {
		if rec.RemotePath == "" {
			continue
		}
		// Same floor as local retention, and it matters more offsite: the remote
		// copy is what survives losing the host.
		if kept < minRetainedSnapshots {
			kept++
			if rec.CreatedAt.Before(cutoff) {
				log.Printf("Keeping offsite snapshot %d past retention: it is one of the newest %d",
					rec.ID, minRetainedSnapshots)
			}
			continue
		}
		if !rec.CreatedAt.Before(cutoff) {
			continue
		}
		bucket, key, parseErr := parseS3Location(rec.RemotePath)
		if parseErr != nil {
			log.Printf("Warning: snapshot %d has an unparseable remote location %q: %v", rec.ID, rec.RemotePath, parseErr)
			continue
		}
		if bucket != settings.Bucket {
			// Guard against deleting from a bucket that is no longer ours.
			log.Printf("Warning: snapshot %d lives in bucket %q but offsite is configured for %q; skipping", rec.ID, bucket, settings.Bucket)
			continue
		}
		if err := s3Client.DeleteFile(ctx, s3Client.RelativeKey(key)); err != nil {
			log.Printf("Warning: could not delete remote snapshot %d: %v", rec.ID, err)
			continue
		}
		if rec.LocalPath == "" {
			if err := op.deps.Store.Snapshot.Delete(rec.ID); err != nil {
				log.Printf("Warning: could not delete snapshot record %d: %v", rec.ID, err)
				continue
			}
		} else if err := op.deps.Store.Snapshot.ClearRemoteLocation(rec.ID); err != nil {
			log.Printf("Warning: could not clear remote location of snapshot %d: %v", rec.ID, err)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		log.Printf("Snapshot remote cleanup: removed %d archive(s) older than %d days", deleted, plan.CleanupRemoteDays)
	}
	return nil
}

// parseS3Location splits an s3://bucket/key location.
func parseS3Location(location string) (bucket, key string, err error) {
	const scheme = "s3://"
	if !strings.HasPrefix(location, scheme) {
		return "", "", fmt.Errorf("not an s3:// location")
	}
	rest := strings.TrimPrefix(location, scheme)
	bucket, key, found := strings.Cut(rest, "/")
	if !found {
		return "", "", fmt.Errorf("no key in location")
	}
	return bucket, key, nil
}
