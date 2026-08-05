package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/urfave/cli/v3"
)

// checklistResponse mirrors operations.ChecklistResult (GET /api/checklist).
type checklistResponse struct {
	GeneratedAt string `json:"generatedAt"`
	Health      struct {
		Overall     string `json:"overall"`
		CheckedAt   string `json:"checkedAt"`
		HostHealthy bool   `json:"hostHealthy"`
		FailDetails string `json:"failDetails"`
	} `json:"health"`
	Instances []struct {
		Name           string `json:"name"`
		Version        string `json:"version"`
		Status         string `json:"status"`
		Health         string `json:"health"`
		ParameterGroup string `json:"parameterGroup"`
		// Legacy: a per-instance backup schedule that still exists marks an
		// un-migrated deployment. Backups are otherwise absent from the audit.
		LegacyBackupCron *struct {
			UTCHour int `json:"utcHour"`
		} `json:"legacyBackupCron"`
		SnapshotCoverage struct {
			State    string            `json:"state"`
			Snapshot *checklistArchive `json:"snapshot"`
		} `json:"snapshotCoverage"`
	} `json:"instances"`
	Snapshots struct {
		Scheduled     bool              `json:"scheduled"`
		UTCHour       int               `json:"utcHour"`
		IntervalHours int               `json:"intervalHours"`
		Format        string            `json:"format"`
		LastSnapshot  *checklistArchive `json:"lastSnapshot"`
		Total         int               `json:"total"`
		Copies        struct {
			LocalAndRemote int `json:"localAndRemote"`
			RemoteOnly     int `json:"remoteOnly"`
			LocalOnly      int `json:"localOnly"`
			None           int `json:"none"`
		} `json:"copies"`
		Stale       bool   `json:"stale"`
		StaleDetail string `json:"staleDetail"`
	} `json:"snapshots"`
	Notifications struct {
		Configured []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"configured"`
		LastEvent *checklistNotificationEvent `json:"lastEvent"`
		LastError *checklistNotificationEvent `json:"lastError"`
	} `json:"notifications"`
}

type checklistNotificationEvent struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"createdAt"`
}

// checklistArchive mirrors operations.ChecklistArchive: one snapshot archive.
type checklistArchive struct {
	ID        int    `json:"id"`
	Timestamp string `json:"timestamp"`
	SizeBytes int64  `json:"sizeBytes"`
	Location  string `json:"location"`
	Comment   string `json:"comment"`
}

func (c *Client) checklistAction(ctx context.Context, cmd *cli.Command) error {
	resp, err := c.request("GET", "/api/checklist", nil)
	if err != nil {
		return err
	}

	if cmd.Bool("json") {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, resp, "", "  "); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
		_, _ = fmt.Fprintln(c.out, pretty.String())
		return nil
	}

	var result checklistResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	out := c.out
	_, _ = fmt.Fprintf(out, "ODDK checklist — generated %s\n\n", result.GeneratedAt)

	// Overall (daemon-wide) health line.
	if result.Health.CheckedAt != "" {
		_, _ = fmt.Fprintf(out, "Overall health: %s (last check %s)\n", result.Health.Overall, result.Health.CheckedAt)
	} else {
		_, _ = fmt.Fprintf(out, "Overall health: %s (no health checks recorded yet)\n", result.Health.Overall)
	}
	if result.Health.FailDetails != "" {
		_, _ = fmt.Fprintf(out, "  %s %s\n", glyphBad, result.Health.FailDetails)
	}
	_, _ = fmt.Fprintln(out)

	// Per-instance blocks. Most hosts run one or a few instances, so a detailed
	// vertical block per instance reads better than a wide table.
	if len(result.Instances) == 0 {
		_, _ = fmt.Fprintln(out, "Instances: none found")
	} else {
		_, _ = fmt.Fprintf(out, "Instances (%d)\n", len(result.Instances))

		// detail prints one aligned "  <glyph> <label>  <value>" line under a
		// block; an empty glyph renders as a blank column so labels stay aligned.
		detail := func(glyph, label, value string) {
			if glyph == "" {
				glyph = " "
			}
			_, _ = fmt.Fprintf(out, "  %s %-17s %s\n", glyph, label, value)
		}

		for _, inst := range result.Instances {
			_, _ = fmt.Fprintln(out)

			// Header: status glyph + name · PostgreSQL <ver> · <status>.
			header := inst.Name
			if inst.Version != "" {
				header += " · PostgreSQL " + inst.Version
			}
			if inst.Status != "" {
				header += " · " + inst.Status
			}
			_, _ = fmt.Fprintf(out, "%s %s\n", statusGlyph(inst.Status), header)

			detail(healthGlyph(inst.Health), "health", inst.Health)

			paramGroup := inst.ParameterGroup
			if paramGroup == "" {
				paramGroup = "(none)"
			}
			detail(glyphOK, "parameter group", paramGroup)

			// Backups are legacy and deliberately absent from the audit; an
			// instance's protection is its snapshot coverage. "config-only"
			// means the newest snapshot holds only this instance's
			// configuration — a restore from it would produce no database
			// contents, which must read as a problem, not protection.
			cov := inst.SnapshotCoverage
			switch {
			case cov.State == "covered" && cov.Snapshot != nil:
				detail(glyphOK, "snapshot coverage", fmt.Sprintf("#%d · %s · %s",
					cov.Snapshot.ID, cov.Snapshot.Timestamp, cov.Snapshot.Location))
			case cov.State == "config-only" && cov.Snapshot != nil:
				detail(glyphBad, "snapshot coverage", fmt.Sprintf(
					"configuration-only in #%d — NO data captured", cov.Snapshot.ID))
			case cov.State == "not-captured":
				detail(glyphTodo, "snapshot coverage", "not yet captured (newer than the newest snapshot)")
			default: // "no-snapshots", or an unknown state from a newer daemon
				detail(glyphTodo, "snapshot coverage", "no completed snapshots")
			}

			// The one backup-shaped line left: a still-scheduled per-instance
			// backup cron marks an un-migrated deployment. Silently hiding it
			// would bury the only signal that migration isn't finished.
			if inst.LegacyBackupCron != nil {
				detail(glyphTodo, "legacy backups", fmt.Sprintf(
					"daily backup cron at %02d:00 UTC still scheduled — migrate: oddk snapshot migrate-from-backups",
					inst.LegacyBackupCron.UTCHour))
			}
		}
	}
	_, _ = fmt.Fprintln(out)

	// Snapshots are global (not per-instance) and are what a host migration or
	// disaster recovery actually restores from, so an audit that omitted them
	// could report healthy per-instance backups for a deployment that cannot be
	// rebuilt at all.
	snap := result.Snapshots
	_, _ = fmt.Fprintln(out, "Snapshots (whole deployment)")
	if snap.Scheduled {
		// The format is part of the schedule (physical is the default; a
		// pre-0.1.61 daemon sends none — show nothing rather than guess).
		formatSuffix := ""
		if snap.Format != "" {
			formatSuffix = ", " + snap.Format
		}
		if snap.IntervalHours >= 24 || snap.IntervalHours == 0 {
			_, _ = fmt.Fprintf(out, "  %s scheduled: daily at %02d:00 UTC%s\n", glyphOK, snap.UTCHour, formatSuffix)
		} else {
			_, _ = fmt.Fprintf(out, "  %s scheduled: every %dh from %02d:00 UTC%s\n", glyphOK, snap.IntervalHours, snap.UTCHour, formatSuffix)
		}
	} else {
		_, _ = fmt.Fprintf(out, "  %s not scheduled (oddk snapshot setup-cron --utc-hour <h>)\n", glyphTodo)
	}
	// A failed capture writes no record at all, so showing the newest COMPLETED
	// snapshot with a green glyph would read fine on a schedule that has been
	// failing for weeks. The staleness check is the only thing that surfaces it.
	switch {
	case snap.LastSnapshot != nil && snap.Stale:
		_, _ = fmt.Fprintf(out, "  %s last snapshot: %s (%s) - STALE: %s\n",
			glyphBad, snap.LastSnapshot.Timestamp, snap.LastSnapshot.Location, snap.StaleDetail)
	case snap.LastSnapshot != nil:
		_, _ = fmt.Fprintf(out, "  %s last snapshot: %s (%s)\n",
			glyphOK, snap.LastSnapshot.Timestamp, snap.LastSnapshot.Location)
	case snap.Stale:
		_, _ = fmt.Fprintf(out, "  %s last snapshot: never - %s\n", glyphBad, snap.StaleDetail)
	default:
		_, _ = fmt.Fprintf(out, "  %s last snapshot: never\n", glyphTodo)
	}
	if snap.Total > 0 {
		_, _ = fmt.Fprintf(out, "    stored: %s\n", formatStoredCopies(
			snap.Total, snap.Copies.LocalAndRemote, snap.Copies.RemoteOnly, snap.Copies.LocalOnly, snap.Copies.None))
	}
	_, _ = fmt.Fprintln(out)

	// Notifications are global (not per-instance).
	_, _ = fmt.Fprintln(out, "Notifications")
	if len(result.Notifications.Configured) == 0 {
		_, _ = fmt.Fprintln(out, "  none configured")
	} else {
		_, _ = fmt.Fprintf(out, "  %d configured: ", len(result.Notifications.Configured))
		for i, n := range result.Notifications.Configured {
			if i > 0 {
				_, _ = fmt.Fprint(out, ", ")
			}
			_, _ = fmt.Fprintf(out, "%s [%s]", n.Name, n.Type)
		}
		_, _ = fmt.Fprintln(out)
	}

	printEvent := func(label string, event *checklistNotificationEvent) {
		if event == nil {
			_, _ = fmt.Fprintf(out, "  %-11s none\n", label)
			return
		}
		msg := event.Detail
		if msg != "" {
			msg = " — " + msg
		}
		_, _ = fmt.Fprintf(out, "  %-11s %s · %s · %s%s\n", label, event.CreatedAt, event.Name, event.Status, msg)
	}
	printEvent("last event", result.Notifications.LastEvent)
	printEvent("last error", result.Notifications.LastError)

	return nil
}

// Checklist status glyphs: ✓ good/configured, ✗ problem, ○ absent/pending.
const (
	glyphOK   = "✓"
	glyphBad  = "✗"
	glyphTodo = "○"
)

// statusGlyph maps an instance's lifecycle status to a block-header glyph.
func statusGlyph(status string) string {
	switch status {
	case "running":
		return glyphOK
	case "error":
		return glyphBad
	default: // stopped, creating, ...
		return glyphTodo
	}
}

// healthGlyph maps an instance's health verdict to a checklist glyph.
func healthGlyph(health string) string {
	switch health {
	case "ok":
		return glyphOK
	case "failing":
		return glyphBad
	default: // not-checked, unknown
		return glyphTodo
	}
}

// formatStoredCopies renders the stored-snapshot count with a breakdown of
// where the copies live. With a single bucket it collapses to "9 local+s3";
// with several it shows the total plus each non-empty bucket, e.g.
// "9 · 7 local+s3, 1 s3, 1 local". Buckets are mutually exclusive and sum to total.
func formatStoredCopies(total, both, remote, local, none int) string {
	var parts []string
	if both > 0 {
		parts = append(parts, fmt.Sprintf("%d local+s3", both))
	}
	if remote > 0 {
		parts = append(parts, fmt.Sprintf("%d s3", remote))
	}
	if local > 0 {
		parts = append(parts, fmt.Sprintf("%d local", local))
	}
	if none > 0 {
		parts = append(parts, fmt.Sprintf("%d no copies", none))
	}
	switch len(parts) {
	case 0:
		return fmt.Sprintf("%d", total)
	case 1:
		return parts[0]
	default:
		return fmt.Sprintf("%d · %s", total, strings.Join(parts, ", "))
	}
}
