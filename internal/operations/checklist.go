package operations

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
)

// checklistNotificationLogWindow bounds how far back the checklist looks for
// the most recent notification error. Events older than this window are not
// scanned, so LastError may be nil even if an older error exists.
const checklistNotificationLogWindow = 200

// ChecklistResult is the audit snapshot returned by GET /api/checklist.
type ChecklistResult struct {
	GeneratedAt   string                 `json:"generatedAt"`
	Health        ChecklistHealth        `json:"health"`
	Instances     []ChecklistInstance    `json:"instances"`
	Snapshots     ChecklistSnapshots     `json:"snapshots"`
	Notifications ChecklistNotifications `json:"notifications"`
}

// ChecklistSnapshots summarizes whole-deployment snapshot state. Like
// notifications, this is global rather than per-instance — a snapshot covers
// every instance, so there is exactly one schedule.
//
// It belongs on the checklist because a snapshot is what a host migration or
// disaster recovery actually restores from. An audit that reports healthy
// per-instance backups while no snapshot has ever been taken is describing a
// deployment that cannot be rebuilt.
type ChecklistSnapshots struct {
	Scheduled     bool              `json:"scheduled"`
	UTCHour       int               `json:"utcHour,omitempty"`
	IntervalHours int               `json:"intervalHours,omitempty"`
	Format        string            `json:"format,omitempty"`
	LastSnapshot  *ChecklistArchive `json:"lastSnapshot,omitempty"`
	Total         int               `json:"total"`
	Copies        ChecklistSnapCopy `json:"copies"`

	// Stale is set when a schedule exists but the newest snapshot is older than
	// two intervals. Without it a schedule that has been failing for weeks reads
	// green: the newest COMPLETED snapshot is still shown, and a failed capture
	// writes no record at all, so there is nothing to make the gap visible.
	Stale       bool   `json:"stale"`
	StaleDetail string `json:"staleDetail,omitempty"`
}

// ChecklistSnapCopy breaks snapshots down by where they actually exist, the
// same way BackupCopies does — "we have 30 snapshots" means nothing if they are
// all on the machine that just died.
type ChecklistSnapCopy struct {
	LocalAndRemote int `json:"localAndRemote"`
	RemoteOnly     int `json:"remoteOnly"`
	LocalOnly      int `json:"localOnly"`
	None           int `json:"none"`
}

// ChecklistHealth summarizes the latest daemon-wide health check.
type ChecklistHealth struct {
	Overall     string `json:"overall"` // "healthy", "degraded", "unhealthy", "checking", "unknown"
	CheckedAt   string `json:"checkedAt,omitempty"`
	HostHealthy bool   `json:"hostHealthy"`
	FailDetails string `json:"failDetails,omitempty"`
}

// ChecklistInstance is the per-instance audit row.
//
// Per-instance backups are a legacy feature and deliberately absent from the
// audit: the protection question is answered by snapshot coverage alone. The
// one backup-shaped field left is LegacyBackupCron — a per-instance backup
// schedule that still exists marks an un-migrated deployment, and an audit
// that silently hid it would bury the one signal that migration isn't done.
type ChecklistInstance struct {
	Name             string               `json:"name"`
	Version          string               `json:"version"`
	Status           string               `json:"status"`
	Health           string               `json:"health"` // "ok", "failing", "not-checked", "unknown"
	ParameterGroup   string               `json:"parameterGroup"`
	LegacyBackupCron *ChecklistBackupCron `json:"legacyBackupCron,omitempty"`
	SnapshotCoverage ChecklistCoverage    `json:"snapshotCoverage"`
}

// ChecklistCoverage reports whether this instance's DATA is in the newest
// completed snapshot. States:
//
//   - "covered":      the newest snapshot holds this instance's data.
//   - "config-only":  the newest snapshot holds only this instance's
//     configuration (it was stopped during a logical capture, or its container
//     was missing/unsafe during a physical one) — a restore from it would
//     produce no database contents, so this must NOT read as protection.
//   - "not-captured": the instance post-dates the newest snapshot (also the
//     case for an instance destroyed and re-created under the same name — the
//     archive holds the predecessor's data, not this incarnation's).
//   - "no-snapshots": no snapshot has ever completed.
//
// Snapshot names which instances had data since migration 019; for an older
// newest-record the state is decided by creation-time alone (optimistically
// "covered"), which self-corrects at the first post-upgrade capture.
type ChecklistCoverage struct {
	State    string            `json:"state"`
	Snapshot *ChecklistArchive `json:"snapshot,omitempty"` // set for covered/config-only
}

// ChecklistBackupCron describes a still-scheduled per-instance daily backup.
// Legacy: `snapshot migrate-from-backups` removes these; one surviving is an
// outstanding migration task, which is the only reason it is still reported.
type ChecklistBackupCron struct {
	UTCHour           int `json:"utcHour"`
	CleanupLocalDays  int `json:"cleanupLocalDays"`
	CleanupRemoteDays int `json:"cleanupRemoteDays"`
}

// ChecklistArchive describes one snapshot archive: the newest completed one
// globally, and the one an instance's coverage refers to.
type ChecklistArchive struct {
	ID        int    `json:"id"`
	Timestamp string `json:"timestamp"`
	SizeBytes int64  `json:"sizeBytes"`
	Location  string `json:"location"` // "local", "s3", "local+s3"
	Comment   string `json:"comment,omitempty"`
}

// ChecklistNotifications summarizes notification configuration and recent
// delivery activity. Notifications are global (not per-instance).
type ChecklistNotifications struct {
	Configured []ChecklistNotificationConfig `json:"configured"`
	LastEvent  *ChecklistNotificationEvent   `json:"lastEvent,omitempty"`
	LastError  *ChecklistNotificationEvent   `json:"lastError,omitempty"`
}

type ChecklistNotificationConfig struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type ChecklistNotificationEvent struct {
	Name      string `json:"name"`
	Status    string `json:"status"` // "success" or "error"
	Detail    string `json:"detail,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// ChecklistOp aggregates a read-only audit snapshot across all instances:
// health, parameter group, snapshot coverage (plus a warning for legacy
// per-instance backup schedules) and global snapshot + notification status.
type ChecklistOp struct {
	deps   *Dependencies
	result *ChecklistResult
}

func NewChecklistOp(deps *Dependencies) *ChecklistOp {
	return &ChecklistOp{deps: deps}
}

func (op *ChecklistOp) Name() string {
	return "Checklist"
}

func (op *ChecklistOp) Type() OpType {
	return OpTypeRead
}

func (op *ChecklistOp) Execute(ctx context.Context) error {
	result := &ChecklistResult{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Instances:   []ChecklistInstance{},
	}

	healthyNames, brokenNames, hasHealthRecord, err := op.collectHealth(&result.Health)
	if err != nil {
		return err
	}

	// Per-instance backup crons are legacy; one still existing is an
	// un-migrated deployment, surfaced as a warning on the instance row.
	plans, err := op.deps.Store.Cron.ListPlans()
	if err != nil {
		return fmt.Errorf("list cron plans: %w", err)
	}
	legacyCronByInstance := make(map[string]*ChecklistBackupCron, len(plans))
	for _, plan := range plans {
		legacyCronByInstance[plan.InstanceName] = &ChecklistBackupCron{
			UTCHour:           plan.UTCHour,
			CleanupLocalDays:  plan.CleanupLocalDays,
			CleanupRemoteDays: plan.CleanupRemoteDays,
		}
	}

	// Collected before the instance loop because each instance row needs the
	// newest completed snapshot to decide its coverage.
	newestSnapshot, err := op.collectSnapshots(&result.Snapshots)
	if err != nil {
		return err
	}

	instanceList, err := op.deps.Store.Instances.List()
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}

	for i := range instanceList {
		// Reconcile stored status with Docker, same as ListRDBMS.
		checkOp := NewConsistencyCheckOp(op.deps, &instanceList[i])
		if err := checkOp.Execute(ctx); err != nil {
			log.Printf("Error checking instance %s: %v", instanceList[i].Name, err)
		} else {
			instanceList[i] = *checkOp.GetInstance()
		}

		inst := instanceList[i]

		health := "not-checked"
		switch {
		case !hasHealthRecord:
			health = "unknown"
		case healthyNames[inst.Name]:
			health = "ok"
		case brokenNames[inst.Name]:
			health = "failing"
		}

		row := ChecklistInstance{
			Name:             inst.Name,
			Version:          inst.Version,
			Status:           inst.Status,
			Health:           health,
			ParameterGroup:   inst.ParameterGroup,
			LegacyBackupCron: legacyCronByInstance[inst.Name],
			SnapshotCoverage: snapshotCoverage(newestSnapshot, result.Snapshots.LastSnapshot,
				inst.Name, inst.CreatedAt.Time),
		}

		result.Instances = append(result.Instances, row)
	}

	if err := op.collectNotifications(&result.Notifications); err != nil {
		return err
	}

	op.result = result
	return nil
}

// collectSnapshots fills the deployment-wide snapshot summary and returns the
// newest completed snapshot record (nil when none has ever completed), which
// the per-instance rows use to decide snapshot coverage.
func (op *ChecklistOp) collectSnapshots(out *ChecklistSnapshots) (*snapshotstore.Record, error) {
	plan, err := op.deps.Store.Snapshot.GetPlan()
	if err != nil {
		return nil, err
	}
	if plan != nil {
		out.Scheduled = true
		out.UTCHour = plan.UTCHour
		out.IntervalHours = plan.IntervalHours
		out.Format = plan.Format
	}

	records, err := op.deps.Store.Snapshot.List()
	if err != nil {
		return nil, err
	}
	out.Total = len(records)

	// A recorded local location only counts as a copy if the archive is
	// actually on disk — it may have been deleted outside ODDK (the catalogue
	// only reconciles paths during `snapshot apply`). Same rule the backup
	// catalogue applies via FileExists; an audit reporting a local copy that
	// is not there would be describing a restore that cannot happen.
	hasLocalFile := func(rec *snapshotstore.Record) bool {
		if rec.LocalPath == "" {
			return false
		}
		_, statErr := os.Stat(rec.LocalPath)
		return statErr == nil
	}

	for _, rec := range records {
		switch {
		case hasLocalFile(rec) && rec.RemotePath != "":
			out.Copies.LocalAndRemote++
		case rec.RemotePath != "":
			out.Copies.RemoteOnly++
		case hasLocalFile(rec):
			out.Copies.LocalOnly++
		default:
			out.Copies.None++
		}
	}

	// List() is newest-first, so the first completed record WITH A SURVIVING
	// COPY is the newest restorable one. A completed record whose archive
	// exists nowhere (local file deleted outside ODDK, no remote) must not
	// drive the audit: a "covered" verdict or a green last-snapshot line
	// naming an archive with zero copies would describe a restore that cannot
	// happen. Such records still show up in the "no copies" bucket above.
	var newest *snapshotstore.Record
	for _, rec := range records {
		if rec.Status != "completed" {
			continue
		}
		if !hasLocalFile(rec) && rec.RemotePath == "" {
			continue
		}
		out.LastSnapshot = &ChecklistArchive{
			ID:        rec.ID,
			Timestamp: rec.CreatedAt.UTC().Format(time.RFC3339),
			SizeBytes: rec.Size,
			Location:  locationLabel(hasLocalFile(rec), rec.RemotePath != ""),
			Comment:   rec.CommentStr,
		}
		newest = rec
		break
	}

	// A failed capture writes no record, so the catalogue alone cannot show that
	// snapshots have stopped happening — only comparing the newest one against
	// the schedule can. Two intervals of slack absorbs the jitter ladder and one
	// missed run without crying wolf.
	if plan != nil {
		interval := time.Duration(plan.IntervalHours) * time.Hour
		if interval <= 0 {
			interval = 24 * time.Hour
		}
		switch {
		case newest == nil:
			out.Stale = true
			out.StaleDetail = "snapshots are scheduled but none with a surviving copy has ever completed"
		case time.Since(newest.CreatedAt.Time) > 2*interval:
			out.Stale = true
			out.StaleDetail = fmt.Sprintf("newest snapshot is %s old, more than two %s intervals",
				time.Since(newest.CreatedAt.Time).Round(time.Hour), interval)
		}
	}
	return newest, nil
}

// snapshotCoverage decides an instance's ChecklistCoverage against the newest
// completed snapshot (see ChecklistCoverage for the state vocabulary).
//
// The creation-time comparison is exact for "did this instance exist when the
// capture began": captures and instance creation serialize through the
// process-wide executor, and `restore-instance` backdates a re-created row to
// its snapshot's capture time. It also catches the same-name trap — an
// instance destroyed and re-created after the capture is YOUNGER than the
// snapshot, whose entry under that name holds the predecessor's data.
func snapshotCoverage(newest *snapshotstore.Record, ref *ChecklistArchive,
	instanceName string, createdAt time.Time,
) ChecklistCoverage {
	if newest == nil {
		return ChecklistCoverage{State: "no-snapshots"}
	}
	if newest.CreatedAt.Before(createdAt) {
		return ChecklistCoverage{State: "not-captured"}
	}

	// Pre-019 record: the archive predates per-instance bookkeeping, so
	// data-presence is unknowable. Report optimistically — the state becomes
	// precise at the first capture recorded by this version.
	if newest.Instances == nil {
		return ChecklistCoverage{State: "covered", Snapshot: ref}
	}

	for _, entry := range newest.Instances {
		if entry.Name != instanceName {
			continue
		}
		if entry.HasData {
			return ChecklistCoverage{State: "covered", Snapshot: ref}
		}
		return ChecklistCoverage{State: "config-only", Snapshot: ref}
	}
	// Existed at capture time yet absent from the entry list — nothing of it
	// is in the archive, so it is not captured.
	return ChecklistCoverage{State: "not-captured"}
}

// locationLabel names where a copy exists, matching the per-instance backup
// vocabulary the checklist already uses.
func locationLabel(hasLocal, hasRemote bool) string {
	switch {
	case hasLocal && hasRemote:
		return "local+s3"
	case hasRemote:
		return "s3"
	case hasLocal:
		return "local"
	default:
		return "none"
	}
}

// collectHealth fills the health summary and returns per-instance name sets
// from the latest health record (nil-safe: hasRecord is false when no health
// check has ever run).
func (op *ChecklistOp) collectHealth(out *ChecklistHealth) (healthy, broken map[string]bool, hasRecord bool, err error) {
	record, err := op.deps.Store.Health.GetLatestHealthRecord()
	if err != nil {
		return nil, nil, false, fmt.Errorf("get latest health record: %w", err)
	}

	if record == nil {
		out.Overall = "unknown"
		return nil, nil, false, nil
	}

	out.CheckedAt = record.GetTimestamp().UTC().Format(time.RFC3339)
	out.HostHealthy = record.HealthyHost
	out.FailDetails = record.FailDetails
	switch {
	case record.InProgress:
		out.Overall = "checking"
	case record.HealthyAll:
		out.Overall = "healthy"
	case record.HealthyHost:
		out.Overall = "degraded"
	default:
		out.Overall = "unhealthy"
	}

	healthy = make(map[string]bool)
	for _, name := range record.GetHealthyInstancesList() {
		healthy[name] = true
	}
	broken = make(map[string]bool)
	for _, name := range record.GetBrokenInstancesList() {
		broken[name] = true
	}
	return healthy, broken, true, nil
}

func (op *ChecklistOp) collectNotifications(out *ChecklistNotifications) error {
	configured, err := op.deps.Store.Notifications.List()
	if err != nil {
		return fmt.Errorf("list notifications: %w", err)
	}
	out.Configured = []ChecklistNotificationConfig{}
	for _, n := range configured {
		out.Configured = append(out.Configured, ChecklistNotificationConfig{
			Name: n.Name,
			Type: string(n.Type),
		})
	}

	logs, err := op.deps.Store.Notifications.GetLogs(checklistNotificationLogWindow)
	if err != nil {
		return fmt.Errorf("get notification logs: %w", err)
	}
	for i, entry := range logs { // ordered created_at DESC
		detail := ""
		if entry.Status == "error" && entry.Error != nil {
			detail = *entry.Error
		} else if entry.Message != nil {
			detail = *entry.Message
		}
		event := &ChecklistNotificationEvent{
			Name:      entry.NotificationName,
			Status:    entry.Status,
			Detail:    detail,
			CreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339),
		}
		if i == 0 {
			out.LastEvent = event
		}
		if entry.Status == "error" {
			out.LastError = event
			break
		}
	}
	return nil
}

func (op *ChecklistOp) GetResult() *ChecklistResult {
	return op.result
}
