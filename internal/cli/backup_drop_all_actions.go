package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/urfave/cli/v3"
)

// 'backup dangerously-drop-all' decommissions the legacy per-instance backup
// system in one sweep: every archive, every S3 copy, the whole catalogue.
// Like migrate-from-backups it is transitional tooling for the
// backups-to-snapshots move; unlike it, the destructive act is a single new
// daemon endpoint (DELETE /api/backups) rather than a client-side loop,
// because the per-ID remove endpoints refuse records whose instance has been
// destroyed — and those leftovers are precisely what this must clear.

type dropAllInstanceSummary struct {
	Name           string `json:"name"`
	InstanceExists bool   `json:"instanceExists"`
	Records        int    `json:"records"`
	LocalFiles     int    `json:"localFiles"`
	LocalBytes     int64  `json:"localBytes"`
	RemoteObjects  int    `json:"remoteObjects"`
}

// dropAllDaemonResult mirrors operations.DropAllBackupsResult.
type dropAllDaemonResult struct {
	RecordsTotal               int      `json:"recordsTotal"`
	RecordsDropped             int      `json:"recordsDropped"`
	RecordsKept                int      `json:"recordsKept"`
	LocalFilesDeleted          int      `json:"localFilesDeleted"`
	LocalFilesMissing          int      `json:"localFilesMissing"`
	LocalBytesFreed            int64    `json:"localBytesFreed"`
	RemoteObjectsDeleted       int      `json:"remoteObjectsDeleted"`
	RemoteRefsDroppedNoOffsite int      `json:"remoteRefsDroppedNoOffsite"`
	Errors                     []string `json:"errors"`
	Message                    string   `json:"message"`
}

type dropAllReport struct {
	DryRun                   bool                     `json:"dryRun"`
	RecordsTotal             int                      `json:"recordsTotal"`
	Instances                []dropAllInstanceSummary `json:"instances"`
	RemainingBackupSchedules []string                 `json:"remainingBackupSchedules"`
	HasCompletedSnapshot     bool                     `json:"hasCompletedSnapshot"`
	OffsiteConfigured        bool                     `json:"offsiteConfigured"`
	Result                   *dropAllDaemonResult     `json:"result,omitempty"`
}

func (c *Client) backupDangerouslyDropAllAction(ctx context.Context, cmd *cli.Command) error {
	apply := cmd.Bool("apply")
	if cmd.Bool("yes") && !apply {
		return fmt.Errorf("--yes only makes sense with --apply: a run without --apply is a preview and prompts for nothing")
	}
	// Same rule as migrate-from-backups: a prompted --json run would write the
	// confirmation prompt into output no parser can read.
	if cmd.Bool("json") && apply && !cmd.Bool("yes") {
		return fmt.Errorf("--json with --apply needs --yes: the confirmation prompt would otherwise be written into the JSON output")
	}

	report, err := c.buildDropAllReport(!apply)
	if err != nil {
		return err
	}

	human := !cmd.Bool("json")
	if human {
		c.printDropAllPreview(report)
	}

	if !apply {
		if human {
			_, _ = fmt.Fprintln(c.out, "\nPreview only — nothing was changed. Re-run with --apply to delete.")
			return nil
		}
		return c.emitDropAllJSON(report)
	}

	// Nothing to confirm when the catalogue is already empty, but the DELETE
	// still runs so a fleet rollout gets the same JSON shape on every host.
	if report.RecordsTotal > 0 && !cmd.Bool("yes") {
		confirmed, err := c.cliConfirm(fmt.Sprintf(
			"\nDelete ALL %d backup record(s) listed above — local archives, offsite copies, and history? [y/N]: ",
			report.RecordsTotal))
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(c.out, "Cancelled")
			return nil
		}
	}

	resp, err := c.request("DELETE", "/api/backups", nil)
	if err != nil {
		return err
	}
	var result dropAllDaemonResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	report.Result = &result

	if human {
		c.printDropAllResult(&result)
	} else if err := c.emitDropAllJSON(report); err != nil {
		return err
	}
	if result.RecordsKept > 0 {
		return fmt.Errorf("%d backup record(s) could not be fully removed — fix the cause and re-run", result.RecordsKept)
	}
	return nil
}

func (c *Client) buildDropAllReport(dryRun bool) (*dropAllReport, error) {
	backups, err := c.fetchAllBackups()
	if err != nil {
		return nil, err
	}
	plans, err := c.fetchBackupCronPlans()
	if err != nil {
		return nil, err
	}
	resp, err := c.request("GET", "/api/checklist", nil)
	if err != nil {
		return nil, err
	}
	var checklist checklistResponse
	if err := json.Unmarshal(resp, &checklist); err != nil {
		return nil, fmt.Errorf("parse checklist: %w", err)
	}

	report := &dropAllReport{
		DryRun:                   dryRun,
		RecordsTotal:             len(backups),
		Instances:                []dropAllInstanceSummary{},
		RemainingBackupSchedules: []string{},
		HasCompletedSnapshot:     checklist.Snapshots.LastSnapshot != nil,
	}
	for _, p := range plans {
		report.RemainingBackupSchedules = append(report.RemainingBackupSchedules, p.InstanceName)
	}

	live := make(map[string]bool, len(checklist.Instances))
	for _, inst := range checklist.Instances {
		live[inst.Name] = true
	}
	perInstance := map[string]*dropAllInstanceSummary{}
	remoteTotal := 0
	for _, b := range backups {
		s := perInstance[b.InstanceName]
		if s == nil {
			s = &dropAllInstanceSummary{Name: b.InstanceName, InstanceExists: live[b.InstanceName]}
			perInstance[b.InstanceName] = s
		}
		s.Records++
		if b.LocalLocation != "" {
			s.LocalFiles++
			s.LocalBytes += b.Size
		}
		if b.RemoteLocation != "" {
			s.RemoteObjects++
			remoteTotal++
		}
	}
	for _, s := range perInstance {
		report.Instances = append(report.Instances, *s)
	}
	sort.Slice(report.Instances, func(i, j int) bool {
		return report.Instances[i].Name < report.Instances[j].Name
	})

	// Whether offsite is configured decides what happens to remote copies
	// (deleted from S3 vs. dropped from the catalogue with the objects left
	// behind), so only ask when the answer matters.
	if remoteTotal > 0 {
		resp, err := c.request("GET", "/api/offsite", nil)
		if err != nil {
			return nil, err
		}
		var offsiteInfo struct {
			Active bool `json:"active"`
		}
		if err := json.Unmarshal(resp, &offsiteInfo); err != nil {
			return nil, fmt.Errorf("parse offsite info: %w", err)
		}
		report.OffsiteConfigured = offsiteInfo.Active
	}
	return report, nil
}

func (c *Client) printDropAllPreview(report *dropAllReport) {
	_, _ = fmt.Fprintln(c.out, "DANGER: this deletes EVERY per-instance backup — local archives, offsite")
	_, _ = fmt.Fprintln(c.out, "copies, and the entire backup history. Snapshots are not touched.")
	_, _ = fmt.Fprintln(c.out)

	if report.RecordsTotal == 0 {
		_, _ = fmt.Fprintln(c.out, "No backups recorded — nothing to drop.")
	} else {
		_, _ = fmt.Fprintf(c.out, "To drop: %d record(s) across %d instance(s):\n",
			report.RecordsTotal, len(report.Instances))
		remoteTotal := 0
		for _, s := range report.Instances {
			name := s.Name
			if !s.InstanceExists {
				name += " (instance destroyed)"
			}
			line := fmt.Sprintf("  %-32s %d local (%s)", name, s.LocalFiles, humanSize(s.LocalBytes))
			if s.RemoteObjects > 0 {
				line += fmt.Sprintf(" · %d offsite", s.RemoteObjects)
				remoteTotal += s.RemoteObjects
			}
			_, _ = fmt.Fprintln(c.out, line)
		}
		if remoteTotal > 0 {
			if report.OffsiteConfigured {
				_, _ = fmt.Fprintf(c.out, "\n%d offsite cop(ies) will be deleted from S3.\n", remoteTotal)
			} else {
				_, _ = fmt.Fprintf(c.out, "\n⚠  %d offsite cop(ies) are recorded but offsite is NOT configured:\n", remoteTotal)
				_, _ = fmt.Fprintln(c.out, "   their records will be dropped, the S3 objects themselves stay in the")
				_, _ = fmt.Fprintln(c.out, "   bucket (ODDK has no way to reach them).")
			}
		}
	}

	if len(report.RemainingBackupSchedules) > 0 {
		_, _ = fmt.Fprintf(c.out, "\n⚠  %d instance(s) still have a backup schedule: %v\n",
			len(report.RemainingBackupSchedules), report.RemainingBackupSchedules)
		_, _ = fmt.Fprintln(c.out, "   New backups will be created on the next cron run. Move the schedules to")
		_, _ = fmt.Fprintln(c.out, "   snapshots first: oddk snapshot migrate-from-backups")
	}
	if !report.HasCompletedSnapshot {
		_, _ = fmt.Fprintln(c.out, "\n⚠  NO SNAPSHOT EXISTS. These backups are the only restore artifacts this")
		_, _ = fmt.Fprintln(c.out, "   deployment has — after this, nothing can be restored until a snapshot")
		_, _ = fmt.Fprintln(c.out, "   is taken. Run 'oddk snapshot make' first.")
	}
}

func (c *Client) printDropAllResult(result *dropAllDaemonResult) {
	_, _ = fmt.Fprintf(c.out, "\n%s\n", result.Message)
	_, _ = fmt.Fprintf(c.out, "  Local archives deleted: %d (%s)",
		result.LocalFilesDeleted, humanSize(result.LocalBytesFreed))
	if result.LocalFilesMissing > 0 {
		_, _ = fmt.Fprintf(c.out, "; already missing: %d", result.LocalFilesMissing)
	}
	_, _ = fmt.Fprintln(c.out)
	if result.RemoteObjectsDeleted > 0 {
		_, _ = fmt.Fprintf(c.out, "  S3 objects deleted: %d\n", result.RemoteObjectsDeleted)
	}
	if result.RemoteRefsDroppedNoOffsite > 0 {
		_, _ = fmt.Fprintf(c.out, "  Offsite references dropped (objects remain in the bucket): %d\n",
			result.RemoteRefsDroppedNoOffsite)
	}
	if len(result.Errors) > 0 {
		_, _ = fmt.Fprintf(c.out, "\n⚠  %d record(s) were KEPT because a copy could not be removed:\n",
			result.RecordsKept)
		for _, e := range result.Errors {
			_, _ = fmt.Fprintf(c.out, "  - %s\n", e)
		}
	}
}

func (c *Client) emitDropAllJSON(report *dropAllReport) error {
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	_, _ = fmt.Fprintf(c.out, "%s\n", out)
	return nil
}
