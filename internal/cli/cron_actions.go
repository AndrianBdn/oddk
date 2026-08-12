package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/andrianbdn/oddk/internal/operations"
	"github.com/andrianbdn/oddk/internal/store/cron"
)

// cronPhases are the four phases of a scheduled run, in execution order, paired
// with the short column heading each gets. Every phase runs even when an earlier
// one failed — retention must still happen on a night the capture failed — so
// all four are always shown rather than stopping at the first failure.
var cronPhases = []struct {
	Header string
	Status func(*cron.CronLog) *string
	Error  func(*cron.CronLog) *string
}{
	{"CAPTURE", func(l *cron.CronLog) *string { return l.BackupStatus }, func(l *cron.CronLog) *string { return l.BackupError }},
	{"UPLOAD", func(l *cron.CronLog) *string { return l.BackupUploadStatus }, func(l *cron.CronLog) *string { return l.BackupUploadError }},
	{"LOCAL-RET", func(l *cron.CronLog) *string { return l.BackupCleanupStatus }, func(l *cron.CronLog) *string { return l.BackupCleanupError }},
	{"OFFSITE-RET", func(l *cron.CronLog) *string { return l.BackupRemoteCleanupStatus }, func(l *cron.CronLog) *string { return l.BackupRemoteCleanupError }},
}

func (c *Client) cronLogsAction(ctx context.Context, cmd *cli.Command) error {
	query := url.Values{}
	query.Set("limit", fmt.Sprintf("%d", cmd.Int("limit")))
	if instance := cmd.String("instance"); instance != "" {
		query.Set("instance", instance)
	}

	resp, err := c.request("GET", "/api/cron/logs?"+query.Encode(), nil)
	if err != nil {
		return err
	}

	var result operations.CronLogsResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	logs := result.Logs
	if cmd.Bool("failures") {
		var failed []*cron.CronLog
		for _, l := range logs {
			if cronLogHasFailure(l) {
				failed = append(failed, l)
			}
		}
		logs = failed
	}

	if cmd.Bool("json") {
		out, err := json.MarshalIndent(operations.CronLogsResult{Logs: logs}, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		_, _ = fmt.Fprintln(c.out, string(out))
		return nil
	}

	if len(logs) == 0 {
		if cmd.Bool("failures") {
			_, _ = fmt.Fprintln(c.out, "No failed scheduled runs found.")
		} else {
			_, _ = fmt.Fprintln(c.out, "No scheduled runs recorded yet.")
			_, _ = fmt.Fprintln(c.out, "Schedule one with 'oddk snapshot setup-cron --utc-hour <H>'.")
		}
		return nil
	}

	headers := []string{"STARTED", "TARGET"}
	for _, p := range cronPhases {
		headers = append(headers, p.Header)
	}

	rows := make([][]string, 0, len(logs))
	for _, l := range logs {
		row := []string{
			l.StartedAt.Format("2006-01-02 15:04"),
			cronLogTarget(l),
		}
		for _, p := range cronPhases {
			row = append(row, cronPhaseGlyph(p.Status(l), l.CompletedAt == nil))
		}
		rows = append(rows, row)
	}

	if err := writeTable(c.out, headers, rows); err != nil {
		return err
	}

	// The table says WHICH phase failed; the operator still needs WHY, and that
	// reason existed only in the daemon log before this command.
	printed := false
	for _, l := range logs {
		for _, p := range cronPhases {
			errMsg := p.Error(l)
			if errMsg == nil || strings.TrimSpace(*errMsg) == "" {
				continue
			}
			if !printed {
				_, _ = fmt.Fprintln(c.out, "\nFailures:")
				printed = true
			}
			_, _ = fmt.Fprintf(c.out, "  %s  %s  %s: %s\n",
				l.StartedAt.Format("2006-01-02 15:04"), cronLogTarget(l), p.Header, *errMsg)
		}
	}

	_, _ = fmt.Fprintln(c.out, "\n✓ ok   ✗ failed   ○ not run   … in progress")
	return nil
}

// cronLogTarget renders what a run covered. Snapshot runs are recorded under a
// sentinel instance name that is not a real instance, so show them as what they
// are: the whole deployment.
func cronLogTarget(l *cron.CronLog) string {
	if l.InstanceName == operations.SnapshotCronInstance {
		return "(snapshot: all)"
	}
	return l.InstanceName
}

func cronPhaseGlyph(status *string, runIncomplete bool) string {
	if status == nil {
		// A phase with no status on a run with no completed_at belongs to a run
		// that is STILL EXECUTING — not one that was interrupted. A run the
		// daemon died in the middle of does not reach here: startup
		// reconciliation (CronStore.MarkInterruptedRuns) stamps completed_at and
		// writes the literal status "interrupted" plus its reason, which render
		// through the default branch below. So this branch means "in progress",
		// and labelling it "interrupted" told an operator watching a healthy
		// multi-minute capture that the daemon had died — inviting the one
		// response that would actually break it.
		//
		// On a completed run a nil status simply did not apply, e.g. upload with
		// no offsite configured.
		if runIncomplete {
			return "…"
		}
		return "○"
	}
	switch *status {
	case "ok":
		return "✓"
	case "fail":
		return "✗"
	default:
		return *status
	}
}

func cronLogHasFailure(l *cron.CronLog) bool {
	for _, p := range cronPhases {
		if s := p.Status(l); s != nil && *s == "fail" {
			return true
		}
	}
	return false
}
