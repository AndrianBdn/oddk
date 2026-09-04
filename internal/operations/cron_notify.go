package operations

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/andrianbdn/oddk/internal/services"
	"github.com/andrianbdn/oddk/internal/store/cron"
)

// cronPhaseFailure is one failed phase of a scheduled run.
type cronPhaseFailure struct {
	Phase string // "backup", "backup_upload", "backup_cleanup", "backup_remote_cleanup"
	Err   error
}

// cronRunReporter accumulates the outcome of a scheduled run's phases and, when
// the run finishes, tells the operator about it.
//
// Before this existed, NOTHING notified on a failed backup, snapshot or S3
// upload — the only NotificationSender call sites in the tree were the health
// checker and `notify test`. Every cron phase recorded its failure to cron_logs
// and returned nil, and cron_logs had no reader in the API or the CLI, so the
// reason a deployment had stopped protecting itself was reachable only via
// journalctl or a manual sqlite3 session. The failure was discovered by the
// checklist's staleness flag — two full intervals later, and with no cause.
type cronRunReporter struct {
	deps *Dependencies

	// key is the cron_logs.instance_name this run is recorded under: a real
	// instance name, or SnapshotCronInstance for a whole-deployment snapshot.
	key string

	// label names the TARGET of the run, not the run itself — it is read as
	// "the scheduled <what> for <label>". So it is `instance "billing"` for a
	// per-instance backup and "the whole deployment" for a snapshot; naming the
	// latter "the whole-deployment snapshot" produced "the scheduled snapshot
	// for the whole-deployment snapshot".
	label string

	// what names the kind of run, for the notification subject.
	what string // "Backup" | "Snapshot"

	// previousRunFailed drives recovery notifications. It is sampled BEFORE
	// this run's log row is created, so it describes the run before this one.
	previousRunFailed bool

	// archiveProduced is set when this run wrote an archive. A failed capture
	// phase means "nothing was produced" without it and "produced, but not a
	// complete capture" with it — two different problems needing two different
	// responses, so the notification must not conflate them.
	archiveProduced bool

	failures []cronPhaseFailure
}

// newCronRunReporter samples the previous run's outcome. Call it before
// CreateLog, or the "previous run" it finds is this one.
func newCronRunReporter(deps *Dependencies, key, label, what string) *cronRunReporter {
	r := &cronRunReporter{deps: deps, key: key, label: label, what: what}

	prev, err := deps.Store.Cron.GetLatestLogForInstance(key)
	if err != nil {
		// Not fatal: a missing recovery notification is much less bad than a
		// scheduled run that refuses to start over a bookkeeping read.
		log.Printf("Warning: cron: could not read the previous run of %s (%v); recovery notification suppressed", label, err)
		return r
	}
	r.previousRunFailed = prev != nil && cronLogFailed(prev)
	return r
}

// record notes one phase's outcome.
func (r *cronRunReporter) record(phase, status string, cause error) {
	if status == "fail" {
		r.failures = append(r.failures, cronPhaseFailure{Phase: phase, Err: cause})
	}
}

// noteArchive records that this run did write an archive, even if its capture
// phase then failed because some instance could not be captured.
func (r *cronRunReporter) noteArchive() {
	if r != nil {
		r.archiveProduced = true
	}
}

// finish sends at most ONE notification for the whole run: a failure summary, or
// a recovery notice when the previous run failed and this one did not.
//
// One per run, not one per phase — a night where the capture, the upload and
// both cleanups fail is one problem, and four messages for it is how an operator
// learns to filter the channel.
func (r *cronRunReporter) finish(ctx context.Context) {
	switch {
	case len(r.failures) > 0:
		r.notify(ctx, fmt.Sprintf("ODDK Scheduled %s Failed", r.what), r.failureBody())
	case r.previousRunFailed:
		r.notify(ctx, fmt.Sprintf("ODDK Scheduled %s Recovered", r.what),
			fmt.Sprintf("The scheduled %s for %s completed successfully.\n\nThe previous run had failed; no action is needed.",
				strings.ToLower(r.what), r.label))
	}
}

func (r *cronRunReporter) failureBody() string {
	var b strings.Builder
	fmt.Fprintf(&b, "The scheduled %s for %s did not complete cleanly.\n\n",
		strings.ToLower(r.what), r.label)

	for _, f := range r.failures {
		reason := "no reason recorded"
		if f.Err != nil {
			reason = f.Err.Error()
		}
		fmt.Fprintf(&b, "  %s: %s\n", cronPhaseLabel(f.Phase), reason)
	}

	// Every phase runs even when an earlier one failed, so a capture failure
	// does not imply retention was skipped. Say which is which.
	switch {
	case r.capturedNothing():
		fmt.Fprintf(&b, "\nNO NEW ARCHIVE WAS PRODUCED by this run. Existing archives are untouched, and\n"+
			"retention keeps the newest ones regardless of age, so there is still something to\n"+
			"restore from — but this deployment is not gaining new protection until it is fixed.\n")
	case r.capturedPartially():
		fmt.Fprintf(&b, "\nAN ARCHIVE WAS PRODUCED, but it is NOT a complete capture: the instance(s) named\n"+
			"above hold NO database contents in it and would restore as EMPTY clusters. It was\n"+
			"still kept, uploaded and catalogued, because it remains the newest restore point\n"+
			"for every other instance. Run 'oddk checklist' to see which instances are covered.\n")
	}

	fmt.Fprintf(&b, "\nSee 'oddk cron logs' for the full run history, or the daemon log for detail.")
	return b.String()
}

// captureFailed reports whether the phase that produces the archive failed.
func (r *cronRunReporter) captureFailed() bool {
	for _, f := range r.failures {
		if f.Phase == "backup" {
			return true
		}
	}
	return false
}

// capturedNothing reports whether the run failed to produce an archive at all,
// as opposed to producing one and then failing to ship or prune it.
func (r *cronRunReporter) capturedNothing() bool {
	return r.captureFailed() && !r.archiveProduced
}

// capturedPartially reports the middle outcome a snapshot can now reach: the
// archive exists and is worth keeping, but at least one instance's data is not
// in it. Backups have no such state — one instance, one dump, all or nothing.
func (r *cronRunReporter) capturedPartially() bool {
	return r.captureFailed() && r.archiveProduced
}

func (r *cronRunReporter) notify(ctx context.Context, subject, body string) {
	displayName := r.deps.Store.KV.GetDisplayName()
	sender := services.NewNotificationSender(r.deps.Store.Notifications, r.deps.Store)

	err := sender.SendToAll(ctx, fmt.Sprintf("%s (%s)", subject, displayName), body)
	if err == nil {
		return
	}
	// "no notifications configured" is the common case on a fresh install and is
	// not a problem worth a warning every night — the run itself is unaffected
	// either way, so no notification failure may ever fail the cron task.
	if errors.Is(err, services.ErrNoNotificationsConfigured) {
		return
	}
	log.Printf("Warning: cron: could not send the %q notification for %s: %v", subject, r.label, err)
}

// cronPhaseLabel turns a cron_logs column prefix into something readable.
func cronPhaseLabel(phase string) string {
	switch phase {
	case "backup":
		return "capture"
	case "backup_upload":
		return "offsite upload"
	case "backup_cleanup":
		return "local retention"
	case "backup_remote_cleanup":
		return "offsite retention"
	default:
		return phase
	}
}

// cronLogFailed reports whether any phase of a recorded run failed. A run with
// no recorded status at all (interrupted before its first phase, e.g. the daemon
// was killed) counts as failed: it did not do what it was scheduled to do.
func cronLogFailed(l *cron.CronLog) bool {
	if l == nil {
		return false
	}
	statuses := []*string{
		l.BackupStatus, l.BackupUploadStatus,
		l.BackupCleanupStatus, l.BackupRemoteCleanupStatus,
	}
	recorded := false
	for _, s := range statuses {
		if s == nil {
			continue
		}
		recorded = true
		if *s == "fail" {
			return true
		}
	}
	return !recorded
}
