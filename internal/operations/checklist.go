package operations

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/andrianbdn/oddk/internal/rfc3339time"
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
	LastSnapshot  *ChecklistBackup  `json:"lastSnapshot,omitempty"`
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
type ChecklistInstance struct {
	Name             string                `json:"name"`
	Version          string                `json:"version"`
	Status           string                `json:"status"`
	Health           string                `json:"health"` // "ok", "failing", "not-checked", "unknown"
	ParameterGroup   string                `json:"parameterGroup"`
	BackupCron       *ChecklistBackupCron  `json:"backupCron,omitempty"`
	LastGoodBackup   *ChecklistBackup      `json:"lastGoodBackup,omitempty"`
	CompletedBackups int                   `json:"completedBackups"`
	BackupCopies     ChecklistBackupCopies `json:"backupCopies"`
}

// ChecklistBackupCopies breaks the completed backups down by where their copies
// live. The four buckets are mutually exclusive and sum to CompletedBackups.
// "None" counts completed records whose local file is gone and that have no
// remote copy (an orphaned record worth surfacing). A local copy only counts
// when the archive is actually present on disk (see the FileExists check below).
type ChecklistBackupCopies struct {
	Both   int `json:"both"`   // local + s3
	Remote int `json:"remote"` // s3 only
	Local  int `json:"local"`  // local only
	None   int `json:"none"`   // completed record, no copy on disk or s3
}

// ChecklistBackupCron describes the scheduled daily backup, if any.
type ChecklistBackupCron struct {
	UTCHour           int `json:"utcHour"`
	CleanupLocalDays  int `json:"cleanupLocalDays"`
	CleanupRemoteDays int `json:"cleanupRemoteDays"`
}

// ChecklistBackup describes the most recent completed backup of an instance.
type ChecklistBackup struct {
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
// health, parameter group, backup state and global notification status.
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

	plans, err := op.deps.Store.Cron.ListPlans()
	if err != nil {
		return fmt.Errorf("list cron plans: %w", err)
	}
	cronByInstance := make(map[string]*ChecklistBackupCron, len(plans))
	for _, plan := range plans {
		cronByInstance[plan.InstanceName] = &ChecklistBackupCron{
			UTCHour:           plan.UTCHour,
			CleanupLocalDays:  plan.CleanupLocalDays,
			CleanupRemoteDays: plan.CleanupRemoteDays,
		}
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
			Name:           inst.Name,
			Version:        inst.Version,
			Status:         inst.Status,
			Health:         health,
			ParameterGroup: inst.ParameterGroup,
			BackupCron:     cronByInstance[inst.Name],
		}

		backups, err := op.deps.Store.Backup.ListBackups(inst.Name)
		if err != nil {
			return fmt.Errorf("list backups for %s: %w", inst.Name, err)
		}
		for _, b := range backups {
			if b.Status != "completed" {
				continue
			}
			row.CompletedBackups++
			// A recorded local location only counts as a copy if the file is
			// actually on disk (it may have been deleted outside ODDK while a
			// remote copy keeps the record alive).
			hasLocal := b.LocalPath != "" && b.FileExists
			hasRemote := b.RemotePath != ""
			switch {
			case hasLocal && hasRemote:
				row.BackupCopies.Both++
			case hasRemote:
				row.BackupCopies.Remote++
			case hasLocal:
				row.BackupCopies.Local++
			default:
				row.BackupCopies.None++
			}
			if row.LastGoodBackup == nil { // list is ordered timestamp DESC
				location := "none"
				switch {
				case hasLocal && hasRemote:
					location = "local+s3"
				case hasLocal:
					location = "local"
				case hasRemote:
					location = "s3"
				}
				row.LastGoodBackup = &ChecklistBackup{
					ID:        b.ID,
					Timestamp: b.Timestamp.UTC().Format(time.RFC3339),
					SizeBytes: b.Size,
					Location:  location,
					Comment:   b.CommentStr,
				}
			}
		}

		result.Instances = append(result.Instances, row)
	}

	if err := op.collectSnapshots(&result.Snapshots); err != nil {
		return err
	}

	if err := op.collectNotifications(&result.Notifications); err != nil {
		return err
	}

	op.result = result
	return nil
}

// collectSnapshots fills the deployment-wide snapshot summary.
func (op *ChecklistOp) collectSnapshots(out *ChecklistSnapshots) error {
	plan, err := op.deps.Store.Snapshot.GetPlan()
	if err != nil {
		return err
	}
	if plan != nil {
		out.Scheduled = true
		out.UTCHour = plan.UTCHour
		out.IntervalHours = plan.IntervalHours
		out.Format = plan.Format
	}

	records, err := op.deps.Store.Snapshot.List()
	if err != nil {
		return err
	}
	out.Total = len(records)

	for _, rec := range records {
		switch {
		case rec.LocalPath != "" && rec.RemotePath != "":
			out.Copies.LocalAndRemote++
		case rec.RemotePath != "":
			out.Copies.RemoteOnly++
		case rec.LocalPath != "":
			out.Copies.LocalOnly++
		default:
			out.Copies.None++
		}
	}

	// List() is newest-first, so the first completed record is the newest one.
	var newest *rfc3339time.Time
	for _, rec := range records {
		if rec.Status != "completed" {
			continue
		}
		out.LastSnapshot = &ChecklistBackup{
			ID:        rec.ID,
			Timestamp: rec.CreatedAt.Format(time.RFC3339),
			SizeBytes: rec.Size,
			Location:  locationLabel(rec.LocalPath != "", rec.RemotePath != ""),
			Comment:   rec.CommentStr,
		}
		created := rec.CreatedAt
		newest = &created
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
			out.StaleDetail = "snapshots are scheduled but none has ever completed"
		case time.Since(newest.Time) > 2*interval:
			out.Stale = true
			out.StaleDetail = fmt.Sprintf("newest snapshot is %s old, more than two %s intervals",
				time.Since(newest.Time).Round(time.Hour), interval)
		}
	}
	return nil
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
