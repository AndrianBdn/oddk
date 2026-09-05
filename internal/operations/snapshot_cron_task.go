package operations

import (
	"context"
	"fmt"
	"log"
	"os"
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
	reporter   *cronRunReporter
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
	// Sample the previous run BEFORE creating this run's row, or the "previous
	// run" the reporter finds is this one.
	op.reporter = newCronRunReporter(op.deps, SnapshotCronInstance, "the whole deployment", "Snapshot")

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
	}

	// Ship whatever archive exists, INCLUDING one whose capture phase failed
	// because some instance could not be captured. A degraded archive is still
	// the newest restore point for every other instance, and leaving its only
	// copy on the host is exactly backwards — the host is the thing offsite
	// copies exist to survive. runSnapshot sets snapshotID whenever an archive
	// was produced, which is the difference between "partial" and "nothing".
	if op.snapshotID != 0 {
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

	// Last, and never fatal: a notification problem must not turn a successful
	// snapshot into a failed cron task.
	op.reporter.finish(ctx)
	return nil
}

// phase records one phase's outcome on the cron log, and on the run reporter
// that decides whether the operator hears about this run.
func (op *SnapshotCronTaskOp) phase(name, status string, cause error) {
	op.set(name+"_status", status)
	op.set(name+"_finished_at", time.Now().UTC())
	if cause != nil {
		op.set(name+"_error", cause.Error())
	}
	if op.reporter != nil {
		op.reporter.record(name, status, cause)
	}
}

func (op *SnapshotCronTaskOp) set(column string, value any) {
	if err := op.deps.Store.Cron.UpdateLog(op.cronLogID, map[string]any{column: value}); err != nil {
		log.Printf("Warning: could not update snapshot cron log %d (%s): %v", op.cronLogID, column, err)
	}
}

func (op *SnapshotCronTaskOp) runSnapshot(ctx context.Context) error {
	// The plan carries the format. A read ERROR fails the capture phase — a
	// deployment configured for logical snapshots must never silently receive
	// a physical one because the plan could not be read (retention still runs;
	// no phase failure aborts the chain). A plan REMOVED mid-run is different:
	// that is an ordinary state, and the default format is fine for a run that
	// was already scheduled. SpreadCheckpoint is always set on the scheduled
	// path: nobody is waiting at a prompt, so pg_basebackup's paced checkpoint
	// (which avoids an I/O spike on a live server) is the right trade.
	format := ""
	plan, planErr := op.deps.Store.Snapshot.GetPlan()
	if planErr != nil {
		return fmt.Errorf("read snapshot plan (its format decides how to capture): %w", planErr)
	}
	if plan != nil {
		format = plan.Format
	}
	result, err := MakeSnapshot(ctx, op.deps, &MakeSnapshotParams{
		BackupDir:        op.backupDir,
		Comment:          "scheduled",
		Format:           format,
		SpreadCheckpoint: true,
	})
	if err != nil {
		return err
	}
	op.snapshotID = result.ID
	op.reporter.noteArchive()
	log.Printf("Scheduled snapshot created: %s (%s, %d bytes, %d instance(s) with data, %d configuration-only)",
		result.Path, result.Format, result.Size, result.InstancesWithData, result.ConfigOnly)
	if result.ConfigOnly > 0 {
		// A configuration-only instance restores to an empty cluster, so this
		// must be visible in the operational log, not just on an interactive run.
		log.Printf("WARNING: scheduled snapshot captured %d instance(s) configuration-only; they hold NO database contents",
			result.ConfigOnly)
	}

	// An instance that could not be captured fails the capture PHASE even though
	// the archive exists and is worth keeping. Reporting this run as successful
	// would recreate, one level up, exactly the bug that made capture dispatch on
	// the stored status: a green run over an archive that is silently empty for
	// somebody. Every later phase still runs — the caller does not abort on this.
	return result.CaptureFailureError()
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

	now := time.Now()
	cutoff := now.AddDate(0, 0, -plan.CleanupLocalDays)
	// The complete-archive pin releases a grace period past the retention
	// window, so a permanently degraded deployment cannot hold one archive here
	// forever — see completePinGraceDays.
	pinCutoff := completePinCutoff(now, plan.CleanupLocalDays)
	records, err := op.deps.Store.Snapshot.List()
	if err != nil {
		return err
	}

	// List() is newest-first, so protecting the floor is a matter of counting
	// how many still-present copies we have walked past — see retentionFloor for
	// why a record whose archive is gone must not consume a slot. Degraded
	// archives still fill newest-N, but they cannot evict the last complete one.
	floor := newRetentionFloor(minRetainedSnapshots)
	completeKept := false
	deleted := 0
	reconciled := 0
	for _, rec := range records {
		if rec.LocalPath == "" {
			continue
		}
		present := localArchivePresent("", rec.LocalPath)
		if !present {
			log.Printf("Warning: snapshot %d is catalogued with a local copy at %s but the file is not there; it does not count toward the newest-%d floor",
				rec.ID, rec.LocalPath, minRetainedSnapshots)
		}
		verdict := snapshotRetentionProtects(floor, &completeKept, present,
			snapshotIsComplete(rec), !rec.CreatedAt.Before(pinCutoff))
		if verdict.keep != retentionKeepNone {
			if rec.CreatedAt.Before(cutoff) {
				log.Printf("Keeping local snapshot %d past retention: %s", rec.ID, verdict.keep.reason(minRetainedSnapshots))
			}
			continue
		}
		if !rec.CreatedAt.Before(cutoff) {
			continue
		}
		// The only-copy safeguard protects a real copy. A record with no file
		// behind it protects nothing, and holding it would strand the row
		// forever: it can never be uploaded, so the condition can never clear.
		if present && offsiteConfigured && rec.RemotePath == "" {
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
		if verdict.pinExpired {
			// The last archive in which every instance had data is going away.
			// Say so explicitly: the deployment has been capturing degraded
			// archives for longer than the pin's grace period, and after this
			// there is no fully restorable local copy at all.
			log.Printf("Warning: local snapshot %d was the newest COMPLETE archive, but every capture since has been degraded for more than %d days past the %d-day retention window, so the pin holding it has expired and it is being removed. Run 'oddk checklist' to see which instance is configuration-only; once it is captured again the next snapshot restores this protection.",
				rec.ID, completePinGraceDays, plan.CleanupLocalDays)
		}
		if err := op.removeLocalSnapshot(rec); err != nil {
			log.Printf("Warning: could not remove local snapshot %d: %v", rec.ID, err)
			continue
		}
		if present {
			deleted++
		} else {
			// removeLocalSnapshot tolerates a missing file, so an aged-out ghost
			// row is repaired here rather than lingering. Counted separately: it
			// freed no disk, and reporting it as a deleted archive would overstate
			// what retention actually pruned.
			reconciled++
		}
	}
	if deleted > 0 {
		log.Printf("Snapshot local cleanup: removed %d archive(s) older than %d days", deleted, plan.CleanupLocalDays)
	}
	if reconciled > 0 {
		log.Printf("Snapshot local cleanup: cleared %d catalogue record(s) whose archive was already gone", reconciled)
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

	now := time.Now()
	cutoff := now.AddDate(0, 0, -plan.CleanupRemoteDays)
	pinCutoff := completePinCutoff(now, plan.CleanupRemoteDays)
	records, err := op.deps.Store.Snapshot.List()
	if err != nil {
		return err
	}

	// Same floor as local retention, and it matters more offsite: the remote copy
	// is what survives losing the host. Existence is checked only while the floor
	// is still filling, so this costs a couple of HeadObject calls per run rather
	// than one per catalogued archive.
	floor := newRetentionFloor(minRetainedSnapshots)
	completeKept := false
	deleted := 0
	for _, rec := range records {
		if rec.RemotePath == "" {
			continue
		}
		complete := snapshotIsComplete(rec)
		// HeadObject only while newest-N is filling, or while the newest
		// complete copy has not been identified yet. The second clause is not
		// narrowed to pin-ELIGIBLE records on purpose: identifying that copy is
		// also what produces the "the pin has expired and the last complete
		// archive is going" warning below, and buying that with one extra
		// HeadObject is worth it. Once both are satisfied, existence cannot
		// change the keep/delete decision.
		needHead := !floor.full() || (!completeKept && complete)
		present := false
		if needHead {
			present = remoteArchivePresent(ctx, s3Client, settings.Bucket, rec.RemotePath)
			if !present {
				log.Printf("Warning: snapshot %d is catalogued with a remote copy at %s but the object is not in the bucket; it does not count toward the newest-%d floor",
					rec.ID, rec.RemotePath, minRetainedSnapshots)
			}
		}
		verdict := snapshotRetentionProtects(floor, &completeKept, present, complete,
			!rec.CreatedAt.Before(pinCutoff))
		if verdict.keep != retentionKeepNone {
			if rec.CreatedAt.Before(cutoff) {
				log.Printf("Keeping offsite snapshot %d past retention: %s", rec.ID, verdict.keep.reason(minRetainedSnapshots))
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
		if verdict.pinExpired {
			log.Printf("Warning: offsite snapshot %d was the newest COMPLETE archive in the bucket, but every capture since has been degraded for more than %d days past the %d-day offsite retention window, so the pin holding it has expired and it is being deleted. Run 'oddk checklist' to see which instance is configuration-only.",
				rec.ID, completePinGraceDays, plan.CleanupRemoteDays)
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
