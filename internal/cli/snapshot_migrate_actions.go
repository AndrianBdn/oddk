package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/urfave/cli/v3"
)

// This command is transitional: it exists to move existing deployments from
// per-instance backup schedules onto the deployment-wide snapshot schedule, and
// should be deleted once every install has been migrated. (Precedent:
// scripts/remote/oddk-migrate.sh, removed in v0.1.45 once it had done its job —
// grep for the command name before deleting, since install.sh still references
// that one.)

// snapshotUploadCapBytes mirrors maxPutObjectBytes in internal/operations: the
// snapshot upload path is a single S3 PutObject. It is duplicated rather than
// imported because this is a pre-flight *estimate* against a remote daemon that
// may be a different build; the daemon remains the authority that enforces it.
const snapshotUploadCapBytes int64 = 5 * 1024 * 1024 * 1024

type backupCronPlan struct {
	InstanceName      string `json:"instanceName"`
	UTCHour           int    `json:"utcHour"`
	CleanupLocalDays  int    `json:"cleanupLocalDays"`
	CleanupRemoteDays int    `json:"cleanupRemoteDays"`
}

// unmanagedBackups counts what stops being pruned when a backup schedule is
// removed. Retention is driven exclusively by the cron plan row, so deleting it
// ends age-based cleanup of that instance's existing archives permanently.
type unmanagedBackups struct {
	Instances  int   `json:"instances"`
	LocalFiles int   `json:"localFiles"`
	LocalBytes int64 `json:"localBytes"`
	RemoteObjs int   `json:"remoteObjects"`
}

type snapshotMigrationReport struct {
	DryRun           bool             `json:"dryRun"`
	SnapshotPlan     snapshotPlan     `json:"snapshotPlan"`
	HadExistingPlan  bool             `json:"hadExistingPlan"`
	KeptExistingPlan bool             `json:"keptExistingPlan"`
	DerivedFrom      []backupCronPlan `json:"derivedFrom"`
	HoursDisagreed   bool             `json:"hoursDisagreed"`
	RemovedSchedules []string         `json:"removedSchedules"`
	Unmanaged        unmanagedBackups `json:"unmanagedBackups"`
	EstimatedBytes   int64            `json:"estimatedSnapshotBytes"`
	EstimateSource   string           `json:"estimateSource"`
	NearUploadCap    bool             `json:"nearUploadCap"`
}

// snapshotMigrateFromBackupsAction adopts the per-instance backup schedules as
// the deployment-wide snapshot schedule and then removes them.
//
// Client-side on purpose: it composes four existing endpoints and needs no new
// daemon surface. The ordering matters — the snapshot schedule is created FIRST,
// so any failure leaves the deployment with both schedules (over-protected)
// rather than neither. That is the house convention for multi-call client
// commands: make the first call harmless on its own.
//
// Offsite settings are deliberately not touched. offsite_settings is a single
// global row that both the backup and snapshot paths already resolve through the
// same helper, so there is nothing per-instance to carry over — only the
// schedule hour and the two retention windows move.
func (c *Client) snapshotMigrateFromBackupsAction(ctx context.Context, cmd *cli.Command) error {
	// The confirmation prompt writes to the same stream as the report, so a
	// prompted --json run would emit something no parser can read. Make the
	// caller say which one they meant rather than silently skipping the work.
	if cmd.Bool("json") && !cmd.Bool("yes") && !cmd.Bool("dry-run") {
		return fmt.Errorf("--json needs --yes (apply) or --dry-run (preview): " +
			"the confirmation prompt would otherwise be written into the JSON output")
	}

	plans, err := c.fetchBackupCronPlans()
	if err != nil {
		return err
	}
	existing, err := c.fetchSnapshotPlan()
	if err != nil {
		return err
	}

	switch {
	case len(plans) == 0 && existing != nil:
		// Idempotent: re-running across a fleet must be a quiet success on hosts
		// that are already done, not an error the operator has to sift out. That
		// is precisely the case a rollout script parses, so this exit has to
		// honour --json like every other one.
		if cmd.Bool("json") {
			return c.emitMigrationJSON(snapshotMigrationReport{
				DryRun:           cmd.Bool("dry-run"),
				SnapshotPlan:     *existing,
				HadExistingPlan:  true,
				KeptExistingPlan: true,
				// Empty rather than nil, so these marshal as [] and a consumer can
				// treat "nothing to migrate" uniformly across both exits.
				DerivedFrom:      []backupCronPlan{},
				RemovedSchedules: []string{},
			})
		}
		_, _ = fmt.Fprintln(c.out, "Already migrated: no backup schedules remain and snapshots are scheduled.")
		printSnapshotPlan(c.out, existing)
		return nil
	case len(plans) == 0:
		return fmt.Errorf("no backup schedules to migrate, and no snapshot schedule configured — " +
			"set one up directly with: oddk snapshot setup-cron --utc-hour <h>")
	}

	report := snapshotMigrationReport{
		DryRun:      cmd.Bool("dry-run"),
		DerivedFrom: plans,
	}
	report.SnapshotPlan, report.HoursDisagreed = derivedSnapshotPlan(plans)
	if existing != nil {
		// Never clobber a schedule someone chose deliberately; adopt it and just
		// retire the backup schedules.
		report.SnapshotPlan = *existing
	}
	// An explicit override is an explicit instruction, so it wins over "keep what
	// is there" — otherwise the preview would show a schedule that is never
	// written. Only an untouched existing plan is left alone.
	overridden := applyPlanOverrides(cmd, &report.SnapshotPlan)
	report.HadExistingPlan = existing != nil
	report.KeptExistingPlan = existing != nil && !overridden

	if err := c.fillMigrationEstimates(plans, &report); err != nil {
		return err
	}
	for _, p := range plans {
		report.RemovedSchedules = append(report.RemovedSchedules, p.InstanceName)
	}

	human := !cmd.Bool("json")
	if human {
		c.printMigrationPreview(report)
	}

	if report.DryRun {
		if human {
			_, _ = fmt.Fprintln(c.out, "\nDry run: nothing was changed.")
			return nil
		}
		return c.emitMigrationJSON(report)
	}
	if !cmd.Bool("yes") {
		confirmed, err := c.cliConfirm(fmt.Sprintf(
			"Adopt this snapshot schedule and remove %d backup schedule(s)? [y/N]: ", len(plans)))
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(c.out, "Cancelled")
			return nil
		}
	}

	if err := c.applyMigration(report, human); err != nil {
		return err
	}
	if !human {
		return c.emitMigrationJSON(report)
	}
	return nil
}

// applyMigration performs the two-phase change: schedule snapshots, then retire
// the backup schedules. A failure part-way through is reported with exactly what
// did happen, because the leftover state is safe but not obvious.
func (c *Client) applyMigration(report snapshotMigrationReport, human bool) error {
	say := func(format string, args ...any) {
		if human {
			_, _ = fmt.Fprintf(c.out, format, args...)
		}
	}

	if !report.KeptExistingPlan {
		body := map[string]int{
			"utcHour":           report.SnapshotPlan.UTCHour,
			"intervalHours":     report.SnapshotPlan.IntervalHours,
			"cleanupLocalDays":  report.SnapshotPlan.CleanupLocalDays,
			"cleanupRemoteDays": report.SnapshotPlan.CleanupRemoteDays,
		}
		if _, err := c.request("POST", "/api/cron/snapshot", body); err != nil {
			return fmt.Errorf("could not schedule snapshots (backup schedules left untouched): %w", err)
		}
		if report.HadExistingPlan {
			say("\nSnapshot schedule updated with your overrides.\n")
		} else {
			say("\nSnapshot schedule configured.\n")
		}
	} else {
		say("\nKeeping the existing snapshot schedule.\n")
	}

	var removed []string
	for _, name := range report.RemovedSchedules {
		if _, err := c.request("DELETE", "/api/cron/backup/"+name, nil); err != nil {
			return fmt.Errorf("snapshots are scheduled, but removing the backup schedule for %q failed: %w\n"+
				"Backup schedules removed so far: %v\n"+
				"The deployment is over-protected, not under-protected — both schedules are active. "+
				"Re-run this command, or remove the rest with: oddk backup setup-cron --instance <name> --remove",
				name, err, removed)
		}
		removed = append(removed, name)
	}
	say("Removed %d backup schedule(s): %v\n", len(removed), removed)

	if human {
		c.printPostMigrationNotes(report)
	}
	return nil
}

// derivedSnapshotPlan folds the per-instance schedules into the single
// deployment-wide one. Hour: the most common, ties broken by the earliest —
// hours were spread to stagger per-instance load, which is meaningless once one
// capture covers everything. Retention: the MAXIMUM, because silently shortening
// someone's retention window during a migration is the one outcome that cannot
// be undone.
func derivedSnapshotPlan(plans []backupCronPlan) (snapshotPlan, bool) {
	counts := map[int]int{}
	for _, p := range plans {
		counts[p.UTCHour]++
	}
	hours := make([]int, 0, len(counts))
	for h := range counts {
		hours = append(hours, h)
	}
	sort.Slice(hours, func(i, j int) bool {
		if counts[hours[i]] != counts[hours[j]] {
			return counts[hours[i]] > counts[hours[j]]
		}
		return hours[i] < hours[j]
	})

	out := snapshotPlan{
		UTCHour:       hours[0],
		IntervalHours: 24, // backup schedules are daily-only; match them
		// A NEW plan gets the daemon's default format. Recorded here so the
		// preview and the JSON report describe the plan that will actually be
		// written (applyMigration itself omits the field and lets the daemon
		// default; an adopted existing plan keeps its own format).
		Format: "physical",
	}
	for _, p := range plans {
		out.CleanupLocalDays = max(out.CleanupLocalDays, p.CleanupLocalDays)
		out.CleanupRemoteDays = max(out.CleanupRemoteDays, p.CleanupRemoteDays)
	}
	return out, len(hours) > 1
}

// applyPlanOverrides applies the operator's explicit flags and reports whether
// any of them changed the plan.
func applyPlanOverrides(cmd *cli.Command, plan *snapshotPlan) bool {
	overridden := false
	if cmd.IsSet("utc-hour") {
		plan.UTCHour = cmd.Int("utc-hour")
		overridden = true
	}
	if cmd.IsSet("interval-hours") {
		plan.IntervalHours = cmd.Int("interval-hours")
		overridden = true
	}
	if cmd.IsSet("cleanup-local-days") {
		plan.CleanupLocalDays = cmd.Int("cleanup-local-days")
		overridden = true
	}
	if cmd.IsSet("cleanup-remote-days") {
		plan.CleanupRemoteDays = cmd.Int("cleanup-remote-days")
		overridden = true
	}
	return overridden
}

func (c *Client) fetchBackupCronPlans() ([]backupCronPlan, error) {
	resp, err := c.request("GET", "/api/cron/backup", nil)
	if err != nil {
		return nil, err
	}
	var plans []backupCronPlan
	if err := json.Unmarshal(resp, &plans); err != nil {
		return nil, fmt.Errorf("parse backup schedules: %w", err)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].InstanceName < plans[j].InstanceName })
	return plans, nil
}

func (c *Client) fetchSnapshotPlan() (*snapshotPlan, error) {
	resp, err := c.request("GET", "/api/cron/snapshot", nil)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Plan *snapshotPlan `json:"plan"`
	}
	if err := json.Unmarshal(resp, &wrapper); err != nil {
		return nil, fmt.Errorf("parse snapshot schedule: %w", err)
	}
	return wrapper.Plan, nil
}

// fillMigrationEstimates works out how big the resulting snapshot is likely to
// be, and how many backups are about to fall out of retention management.
func (c *Client) fillMigrationEstimates(plans []backupCronPlan, report *snapshotMigrationReport) error {
	resp, err := c.request("GET", "/api/checklist", nil)
	if err != nil {
		return err
	}
	var checklist checklistResponse
	if err := json.Unmarshal(resp, &checklist); err != nil {
		return fmt.Errorf("parse checklist: %w", err)
	}

	// A real snapshot beats any estimate derived from per-instance backups.
	if snap := checklist.Snapshots.LastSnapshot; snap != nil && snap.SizeBytes > 0 {
		report.EstimatedBytes = snap.SizeBytes
		report.EstimateSource = "lastSnapshot"
	} else {
		for _, inst := range checklist.Instances {
			if inst.LastGoodBackup != nil {
				report.EstimatedBytes += inst.LastGoodBackup.SizeBytes
			}
		}
		report.EstimateSource = "sumOfBackups"
	}
	report.NearUploadCap = report.EstimatedBytes > snapshotUploadCapBytes*4/5

	migrating := make(map[string]bool, len(plans))
	for _, p := range plans {
		migrating[p.InstanceName] = true
	}

	backups, err := c.fetchAllBackups()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, b := range backups {
		if !migrating[b.InstanceName] {
			continue
		}
		if b.LocalLocation != "" {
			report.Unmanaged.LocalFiles++
			report.Unmanaged.LocalBytes += b.Size
		}
		if b.RemoteLocation != "" {
			report.Unmanaged.RemoteObjs++
		}
		if (b.LocalLocation != "" || b.RemoteLocation != "") && !seen[b.InstanceName] {
			seen[b.InstanceName] = true
			report.Unmanaged.Instances++
		}
	}
	return nil
}

type backupSummary struct {
	ID             int    `json:"id"`
	InstanceName   string `json:"instanceName"`
	Size           int64  `json:"size"`
	LocalLocation  string `json:"localLocation,omitempty"`
	RemoteLocation string `json:"remoteLocation,omitempty"`
}

func (c *Client) fetchAllBackups() ([]backupSummary, error) {
	resp, err := c.request("GET", "/api/backups", nil)
	if err != nil {
		return nil, err
	}
	var backups []backupSummary
	if err := json.Unmarshal(resp, &backups); err != nil {
		return nil, fmt.Errorf("parse backups: %w", err)
	}
	return backups, nil
}

func (c *Client) emitMigrationJSON(report snapshotMigrationReport) error {
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	_, _ = fmt.Fprintf(c.out, "%s\n", out)
	return nil
}

func (c *Client) printMigrationPreview(report snapshotMigrationReport) {
	_, _ = fmt.Fprintf(c.out, "Migrating %d backup schedule(s) to the deployment-wide snapshot schedule.\n\n",
		len(report.DerivedFrom))

	_, _ = fmt.Fprintln(c.out, "Current backup schedules:")
	for _, p := range report.DerivedFrom {
		_, _ = fmt.Fprintf(c.out, "  %-20s %02d:00 UTC · keep local %dd, offsite %dd\n",
			p.InstanceName, p.UTCHour, p.CleanupLocalDays, p.CleanupRemoteDays)
	}
	_, _ = fmt.Fprintln(c.out)

	switch {
	case report.KeptExistingPlan:
		_, _ = fmt.Fprintln(c.out, "A snapshot schedule already exists and will be kept as-is:")
	case report.HadExistingPlan:
		_, _ = fmt.Fprintln(c.out, "The existing snapshot schedule, with your overrides applied:")
	default:
		_, _ = fmt.Fprintln(c.out, "Derived snapshot schedule:")
	}
	printSnapshotPlan(c.out, &report.SnapshotPlan)

	if report.HoursDisagreed && !report.HadExistingPlan {
		_, _ = fmt.Fprintf(c.out,
			"  note: the schedules did not agree on an hour; picked the most common (%02d:00 UTC).\n"+
				"        One capture covers every instance, so staggering no longer applies.\n"+
				"        Override with --utc-hour.\n", report.SnapshotPlan.UTCHour)
	}
	if !report.HadExistingPlan {
		_, _ = fmt.Fprintln(c.out,
			"  note: retention is the LONGEST window any schedule used, so nothing is shortened.")
		if report.EstimatedBytes > 0 {
			_, _ = fmt.Fprintf(c.out, "        Rough local disk at rest: %d days x ~%s = ~%s\n",
				report.SnapshotPlan.CleanupLocalDays, humanSize(report.EstimatedBytes),
				humanSize(report.EstimatedBytes*int64(report.SnapshotPlan.CleanupLocalDays)))
		}
	}

	if report.NearUploadCap {
		_, _ = fmt.Fprintf(c.out,
			"\n⚠  Estimated snapshot size ~%s is near the %d GiB single-PutObject limit for\n"+
				"   offsite upload. Per-instance backup uploads have no such limit, so a\n"+
				"   deployment that uploaded fine before can produce a snapshot that cannot.\n"+
				"   (%s; a snapshot compresses the whole deployment as one stream, so treat\n"+
				"   this as approximate.)\n",
			humanSize(report.EstimatedBytes), snapshotUploadCapBytes/(1024*1024*1024),
			estimateSourceLabel(report.EstimateSource))
	}
}

func estimateSourceLabel(source string) string {
	if source == "lastSnapshot" {
		return "measured from the most recent snapshot"
	}
	return "estimated by summing each instance's newest backup"
}

// printPostMigrationNotes reports what the migration deliberately left alone.
// Removing a backup schedule also permanently ends age-based cleanup of that
// instance's existing archives — retention only ever runs from an existing cron
// plan row — so those archives are now the operator's to manage.
func (c *Client) printPostMigrationNotes(report snapshotMigrationReport) {
	u := report.Unmanaged
	if u.LocalFiles > 0 || u.RemoteObjs > 0 {
		_, _ = fmt.Fprintf(c.out, "\n⚠  %d instance(s) still hold %d local backup(s) (%s)",
			u.Instances, u.LocalFiles, humanSize(u.LocalBytes))
		if u.RemoteObjs > 0 {
			_, _ = fmt.Fprintf(c.out, " and %d offsite copy(ies)", u.RemoteObjs)
		}
		_, _ = fmt.Fprintln(c.out, ".")
		_, _ = fmt.Fprintln(c.out, "   These are NO LONGER PRUNED — retention only runs from a backup schedule,")
		_, _ = fmt.Fprintln(c.out, "   and those are now gone. They stay restorable with 'oddk backup restore'.")
		_, _ = fmt.Fprintln(c.out, "   Keep them until you trust the snapshot schedule, then remove them:")
		_, _ = fmt.Fprintln(c.out, "     oddk backup list")
		_, _ = fmt.Fprintln(c.out, "     oddk backup remove-local <instance> <id>")
	}

	_, _ = fmt.Fprintln(c.out, "\nNext: verify the new schedule end to end with")
	_, _ = fmt.Fprintln(c.out, "  oddk snapshot make && oddk checklist")
}
