package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/moby/term"
	"github.com/urfave/cli/v3"

	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operations"
	s3service "github.com/andrianbdn/oddk/internal/services/s3"
)

type snapshotInstanceEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Image       string `json:"image"`
	HasData     bool   `json:"hasData"`
	CaptureMode string `json:"captureMode,omitempty"`
	SkipReason  string `json:"skipReason,omitempty"`
}

type snapshotMakeResult struct {
	ID                int                     `json:"id"`
	Path              string                  `json:"path"`
	Size              int64                   `json:"size"`
	Timestamp         string                  `json:"timestamp"`
	Format            string                  `json:"format"`
	Instances         []snapshotInstanceEntry `json:"instances"`
	InstancesWithData int                     `json:"instancesWithData"`
	ConfigOnly        int                     `json:"configOnly"`
}

func (c *Client) snapshotMakeAction(ctx context.Context, cmd *cli.Command) error {
	_, _ = fmt.Fprintln(c.out, "Snapshotting deployment (this may take a while)...")

	body := map[string]string{}
	if comment := cmd.String("comment"); comment != "" {
		body["comment"] = comment
	}
	if cmd.Bool("logical") {
		body["format"] = "logical"
	}
	resp, err := c.request("POST", "/api/snapshot", body)
	if err != nil {
		return err
	}

	if cmd.Bool("json") {
		_, _ = fmt.Fprintf(c.out, "%s\n", resp)
		return nil
	}

	var result snapshotMakeResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if len(result.Instances) > 0 {
		_, _ = fmt.Fprintln(c.out)
		w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
		for _, inst := range result.Instances {
			if inst.HasData {
				// A cold capture is a real capture, but the operator should see
				// that the instance was stopped — its restore comes back stopped.
				if inst.CaptureMode == "cold" {
					_, _ = fmt.Fprintf(w, "  %s %s\tPostgreSQL %s (stopped; captured cold)\n", glyphOK, inst.Name, inst.Version)
					continue
				}
				_, _ = fmt.Fprintf(w, "  %s %s\tPostgreSQL %s\n", glyphOK, inst.Name, inst.Version)
				continue
			}
			// Configuration-only entries must be impossible to miss: the
			// instance will come back with no databases.
			_, _ = fmt.Fprintf(w, "  %s %s\t%s\n", glyphTodo, inst.Name, inst.SkipReason)
		}
		_ = w.Flush()
	}

	_, _ = fmt.Fprintln(c.out)
	// The ID is what 'snapshot upload/download/remove-*' take, so printing only
	// the path would mean looking it up with 'snapshot list' every time.
	if result.ID > 0 {
		_, _ = fmt.Fprintf(c.out, "Snapshot: %s (id %d)\n", result.Path, result.ID)
	} else {
		_, _ = fmt.Fprintf(c.out, "Snapshot: %s\n", result.Path)
	}
	// humanSize, matching 'snapshot list' — the same value was rendered two
	// different ways. It also makes the 5 GiB offsite upload limit something an
	// operator can check at a glance. The exact byte count is still in --json.
	_, _ = fmt.Fprintf(c.out, "Size: %s\n", humanSize(result.Size))
	_, _ = fmt.Fprintf(c.out, "Format: %s\n", describeSnapshotFormat(result.Format))
	// The per-instance list above already names every instance, so spelling out
	// "N with data, 0 configuration-only" just restates the total. Break it down
	// only when there is actually a split to report.
	if result.ConfigOnly > 0 {
		_, _ = fmt.Fprintf(c.out, "Instances: %d (%d with data, %d configuration-only)\n",
			len(result.Instances), result.InstancesWithData, result.ConfigOnly)
	} else {
		_, _ = fmt.Fprintf(c.out, "Instances: %d\n", len(result.Instances))
	}

	if result.ConfigOnly > 0 {
		_, _ = fmt.Fprintf(c.out,
			"\n⚠️  %d instance(s) were not running and hold NO database contents in this snapshot.\n",
			result.ConfigOnly)
	}

	// Both of these are load-bearing and easy to get wrong, so they are shown
	// on the success path rather than left to the documentation.
	_, _ = fmt.Fprintf(c.out,
		"\n⚠️  This snapshot contains database contents and role password hashes in plaintext.\n"+
			"   It is NOT encrypted by the master key. Store it accordingly.\n")
	_, _ = fmt.Fprintf(c.out,
		"⚠️  Restoring it requires the master.key from this host. Back that key up separately.\n")

	return nil
}

type snapshotPlan struct {
	UTCHour           int    `json:"utcHour"`
	IntervalHours     int    `json:"intervalHours"`
	CleanupLocalDays  int    `json:"cleanupLocalDays"`
	CleanupRemoteDays int    `json:"cleanupRemoteDays"`
	Format            string `json:"format"`
	UpdatedAt         string `json:"updatedAt"`
}

type snapshotRecord struct {
	ID                int    `json:"id"`
	Filename          string `json:"filename"`
	CreatedAt         string `json:"createdAt"`
	Size              int64  `json:"size"`
	Status            string `json:"status"`
	Format            string `json:"format"`
	InstancesWithData int    `json:"instancesWithData"`
	ConfigOnly        int    `json:"configOnly"`
	LocalLocation     string `json:"localLocation,omitempty"`
	RemoteLocation    string `json:"remoteLocation,omitempty"`
	Comment           string `json:"comment,omitempty"`
}

// describeSnapshotFormat renders a format value for humans, tolerating the
// empty string an old daemon (or an old catalogue row) reports.
func describeSnapshotFormat(format string) string {
	switch format {
	case "physical":
		return "physical (pg_basebackup)"
	case "logical", "":
		return "logical (portable pg_dump)"
	default:
		return format
	}
}

// snapshotSetupCronAction configures (or removes) the deployment-wide snapshot
// schedule. There is at most one — a snapshot covers every instance.
func (c *Client) snapshotSetupCronAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Bool("remove") {
		if _, err := c.request("DELETE", "/api/cron/snapshot", nil); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(c.out, "Scheduled snapshots removed")
		return nil
	}

	if !cmd.IsSet("utc-hour") {
		return fmt.Errorf("--utc-hour is required (or --remove to delete the schedule)")
	}

	// Send ONLY what the operator actually set, so the daemon can merge with the
	// existing plan. Sending the flag defaults unconditionally would make
	// `setup-cron --utc-hour 4` silently reset a 6-hour interval back to daily.
	body := map[string]any{"utcHour": cmd.Int("utc-hour")}
	if cmd.IsSet("interval-hours") {
		body["intervalHours"] = cmd.Int("interval-hours")
	}
	if cmd.IsSet("cleanup-local-days") {
		body["cleanupLocalDays"] = cmd.Int("cleanup-local-days")
	}
	if cmd.IsSet("cleanup-remote-days") {
		body["cleanupRemoteDays"] = cmd.Int("cleanup-remote-days")
	}
	// IsSet-gated like the rest: --logical selects logical, an explicit
	// --logical=false switches an existing plan back to physical, and leaving
	// the flag off preserves whatever the plan already uses.
	if cmd.IsSet("logical") {
		if cmd.Bool("logical") {
			body["format"] = "logical"
		} else {
			body["format"] = "physical"
		}
	}

	resp, err := c.request("POST", "/api/cron/snapshot", body)
	if err != nil {
		return err
	}
	var plan snapshotPlan
	if err := json.Unmarshal(resp, &plan); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	printSnapshotPlan(c.out, &plan)
	return nil
}

func (c *Client) snapshotListCronAction(ctx context.Context, cmd *cli.Command) error {
	resp, err := c.request("GET", "/api/cron/snapshot", nil)
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		_, _ = fmt.Fprintf(c.out, "%s\n", resp)
		return nil
	}

	var wrapper struct {
		Plan *snapshotPlan `json:"plan"`
	}
	if err := json.Unmarshal(resp, &wrapper); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if wrapper.Plan == nil {
		_, _ = fmt.Fprintln(c.out, "No scheduled snapshots configured")
		_, _ = fmt.Fprintln(c.out, "Configure with: oddk snapshot setup-cron --utc-hour 3")
		return nil
	}
	printSnapshotPlan(c.out, wrapper.Plan)
	return nil
}

func printSnapshotPlan(out io.Writer, plan *snapshotPlan) {
	if plan.IntervalHours >= 24 {
		_, _ = fmt.Fprintf(out, "Scheduled snapshot: daily at %02d:00 UTC\n", plan.UTCHour)
	} else {
		_, _ = fmt.Fprintf(out, "Scheduled snapshot: every %d hours, anchored at %02d:00 UTC\n",
			plan.IntervalHours, plan.UTCHour)
		_, _ = fmt.Fprintf(out, "  Runs at: %s UTC\n", snapshotRunHours(plan))
	}
	_, _ = fmt.Fprintf(out, "  Format: %s\n", describeSnapshotFormat(plan.Format))
	_, _ = fmt.Fprintf(out, "  Keep local:  %d days\n", plan.CleanupLocalDays)
	_, _ = fmt.Fprintf(out, "  Keep offsite: %d days\n", plan.CleanupRemoteDays)
}

// snapshotRunHours spells out the hours a sub-daily plan fires, so the operator
// can see the anchor's effect rather than having to compute it.
func snapshotRunHours(plan *snapshotPlan) string {
	if plan.IntervalHours <= 0 {
		return ""
	}
	var hours []string
	for h := range 24 {
		if ((h-plan.UTCHour)%plan.IntervalHours+plan.IntervalHours)%plan.IntervalHours == 0 {
			hours = append(hours, fmt.Sprintf("%02d:00", h))
		}
	}
	return strings.Join(hours, ", ")
}

func (c *Client) snapshotListAction(ctx context.Context, cmd *cli.Command) error {
	resp, err := c.request("GET", "/api/snapshots", nil)
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		_, _ = fmt.Fprintf(c.out, "%s\n", resp)
		return nil
	}

	var records []snapshotRecord
	if err := json.Unmarshal(resp, &records); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if len(records) == 0 {
		_, _ = fmt.Fprintln(c.out, "No snapshots recorded")
		return nil
	}

	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tCREATED\tSIZE\tFORMAT\tINSTANCES\tCOPIES\tCOMMENT")
	for _, r := range records {
		format := r.Format
		if format == "" {
			format = "logical" // pre-0.1.61 rows predate the column
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d+%d\t%s\t%s\n",
			r.ID, r.CreatedAt, humanSize(r.Size), format,
			r.InstancesWithData, r.ConfigOnly,
			copiesLabel(r.LocalLocation, r.RemoteLocation), r.Comment)
	}
	_ = w.Flush()
	return nil
}

// copiesLabel mirrors 'oddk checklist': where a snapshot actually exists is the
// thing an operator needs to see at a glance.
func copiesLabel(local, remote string) string {
	switch {
	case local != "" && remote != "":
		return "local+s3"
	case remote != "":
		return "s3"
	case local != "":
		return "local"
	default:
		return "none"
	}
}

func humanSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

func (c *Client) snapshotUploadAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() != 1 {
		return fmt.Errorf("usage: oddk snapshot upload <snapshot-id>")
	}
	id := cmd.Args().First()

	_, _ = fmt.Fprintf(c.out, "Uploading snapshot %s (this may take a while)...\n", id)
	resp, err := c.request("POST", fmt.Sprintf("/api/snapshot/%s/upload", id), nil)
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		_, _ = fmt.Fprintf(c.out, "%s\n", resp)
		return nil
	}

	var result struct {
		Filename       string `json:"filename"`
		Size           int64  `json:"size"`
		RemoteLocation string `json:"remoteLocation"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	_, _ = fmt.Fprintf(c.out, "Uploaded %s (%s) to %s\n",
		result.Filename, humanSize(result.Size), result.RemoteLocation)
	return nil
}

// snapshotDownloadAction retrieves a snapshot from S3 back onto this host.
//
// Without it the offsite copy is write-only: retention removes the local copy
// and the primary DR artifact can no longer be fetched through ODDK.
func (c *Client) snapshotDownloadAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() != 1 {
		return fmt.Errorf("usage: oddk snapshot download <snapshot-id>")
	}
	id := cmd.Args().First()

	_, _ = fmt.Fprintf(c.out, "Downloading snapshot %s (this may take a while)...\n", id)
	resp, err := c.request("POST", fmt.Sprintf("/api/snapshot/%s/download", id), nil)
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		_, _ = fmt.Fprintf(c.out, "%s\n", resp)
		return nil
	}
	var result struct {
		Filename  string `json:"filename"`
		Size      int64  `json:"size"`
		LocalPath string `json:"localPath"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	_, _ = fmt.Fprintf(c.out, "Downloaded %s (%s) to %s\n",
		result.Filename, humanSize(result.Size), result.LocalPath)
	return nil
}

func (c *Client) snapshotRemoveLocalAction(ctx context.Context, cmd *cli.Command) error {
	return c.snapshotRemoveCopy(cmd, "local")
}

func (c *Client) snapshotRemoveRemoteAction(ctx context.Context, cmd *cli.Command) error {
	return c.snapshotRemoveCopy(cmd, "remote")
}

// snapshotRemoveCopy deletes one copy of a snapshot. When it is the last copy
// the catalogue record goes with it, because a record describing neither a local
// nor a remote copy is forbidden by the schema and would describe nothing.
func (c *Client) snapshotRemoveCopy(cmd *cli.Command, which string) error {
	if cmd.Args().Len() != 1 {
		return fmt.Errorf("usage: oddk snapshot remove-%s <snapshot-id>", which)
	}
	id := cmd.Args().First()

	if !cmd.Bool("force") {
		confirmed, err := c.cliConfirm(fmt.Sprintf(
			"Remove the %s copy of snapshot %s? (if it is the only copy, the record is removed too) [y/N]: ", which, id))
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(c.out, "Cancelled")
			return nil
		}
	}

	if _, err := c.request("DELETE", fmt.Sprintf("/api/snapshot/%s/%s", id, which), nil); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.out, "Removed %s copy of snapshot %s\n", which, id)
	return nil
}

type snapshotArchiveOrigin struct {
	Kind             string `json:"kind"`
	Path             string `json:"path"`
	DownloadedBytes  int64  `json:"downloadedBytes"`
	CredentialSource string `json:"credentialSource"`
}

type snapshotRestoreInstanceResult struct {
	Instance        string                 `json:"instance"`
	Created         bool                   `json:"created"`
	Replaced        bool                   `json:"replaced"`
	Databases       int                    `json:"databases"`
	SourceHost      string                 `json:"sourceHost"`
	SnapshotAt      string                 `json:"snapshotAt"`
	Port            int                    `json:"port"`
	CPUCores        int                    `json:"cpuCores"`
	RAMMB           int                    `json:"ramMb"`
	Image           string                 `json:"image"`
	PasswordChanged bool                   `json:"passwordChanged"`
	Format          string                 `json:"format"`
	FinalStatus     string                 `json:"finalStatus"`
	ArchiveOrigin   *snapshotArchiveOrigin `json:"archiveOrigin"`
}

// snapshotRestoreInstanceAction restores ONE instance out of a snapshot into
// this live deployment.
//
// Unlike `snapshot apply` this goes through the daemon, so --file names a path
// on the DAEMON's filesystem (as `backup restore --file` already does). The
// client therefore cannot preflight the archive itself; the daemon validates and
// refuses before touching anything. The --id and --s3-uri forms skip the
// filesystem question entirely: the daemon fetches the archive itself.
func (c *Client) snapshotRestoreInstanceAction(ctx context.Context, cmd *cli.Command) error {
	instance := cmd.String("instance")
	archivePath := cmd.String("file")
	snapshotID := cmd.Int("id")
	s3URI := cmd.String("s3-uri")
	if instance == "" {
		return fmt.Errorf("--instance is required (which instance to restore out of the snapshot)")
	}
	sources := 0
	for _, set := range []bool{archivePath != "", snapshotID != 0, s3URI != ""} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return fmt.Errorf("exactly one of --file, --id or --s3-uri is required (the snapshot to restore from)")
	}
	if s3URI == "" && (cmd.String("region") != "" || cmd.String("endpoint") != "" || cmd.String("aws-profile") != "") {
		return fmt.Errorf("--region, --endpoint and --aws-profile are only meaningful with --s3-uri")
	}

	sourceDesc := archivePath
	switch {
	case snapshotID != 0:
		sourceDesc = fmt.Sprintf("snapshot id %d (this host's catalogue)", snapshotID)
	case s3URI != "":
		sourceDesc = s3URI
	}

	if !cmd.Bool("yes") {
		_, _ = fmt.Fprintf(c.out, "About to restore instance %q from %s\n\n", instance, sourceDesc)
		_, _ = fmt.Fprintln(c.out, "  - If the instance exists here, its container and DATA VOLUME are destroyed and")
		_, _ = fmt.Fprintln(c.out, "    rebuilt from the snapshot. Data written since the snapshot is lost.")
		_, _ = fmt.Fprintln(c.out, "  - Its port, resources, image and parameter group are set to the SNAPSHOT's,")
		_, _ = fmt.Fprintln(c.out, "    not whatever they are now.")
		_, _ = fmt.Fprintln(c.out, "  - Its postgres password becomes the snapshot's. The archive carries only a")
		_, _ = fmt.Fprintln(c.out, "    hash, so the source's password is the only one that can still authenticate.")
		_, _ = fmt.Fprintln(c.out, "  - Other instances are untouched.")
		_, _ = fmt.Fprintln(c.out)

		confirmed, err := c.cliConfirm(fmt.Sprintf("Restore instance %q from this snapshot? [y/N]: ", instance))
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(c.out, "Cancelled")
			return nil
		}
	}

	body := map[string]any{"instance": instance}
	switch {
	case archivePath != "":
		body["filePath"] = archivePath
	case snapshotID != 0:
		body["snapshotId"] = snapshotID
	default:
		body["s3Uri"] = s3URI
		// Best-effort: attach this shell's resolved ambient credentials so the
		// daemon can reach a bucket its offsite settings do not cover. An empty
		// shell attaches nothing — the daemon's rung order (offsite settings →
		// request → its own instance role) decides what authenticates.
		creds, resolvedRegion, err := c.resolveAmbientAWSCredentials(ctx, cmd.String("aws-profile"))
		if err != nil {
			return err
		}
		if creds != nil {
			body["credentials"] = creds
			c.warnIfRemoteDaemonCreds()
		}
		region := cmd.String("region")
		if region == "" {
			region = resolvedRegion
		}
		if region != "" {
			body["region"] = region
		}
		if endpoint := cmd.String("endpoint"); endpoint != "" {
			body["endpoint"] = endpoint
		}
	}
	if key := cmd.String("master-key"); key != "" {
		body["masterKeyPath"] = key
	}

	if archivePath == "" {
		_, _ = fmt.Fprintf(c.out, "\nFetching the snapshot archive if needed, then restoring %s (this may take a while)...\n", instance)
	} else {
		_, _ = fmt.Fprintf(c.out, "\nRestoring %s (this may take a while)...\n", instance)
	}
	resp, err := c.request("POST", "/api/snapshot/restore-instance", body)
	if err != nil {
		return err
	}

	if cmd.Bool("json") {
		_, _ = fmt.Fprintf(c.out, "%s\n", resp)
		return nil
	}

	var result snapshotRestoreInstanceResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	verb := "Restored"
	if result.Created {
		verb = "Created and restored"
	}
	_, _ = fmt.Fprintf(c.out, "\n%s instance %s\n", verb, result.Instance)
	_, _ = fmt.Fprintf(c.out, "  Databases: %d\n", result.Databases)
	_, _ = fmt.Fprintf(c.out, "  Config:    port %d, %d CPU, %d MB, image %s\n",
		result.Port, result.CPUCores, result.RAMMB, result.Image)
	if result.Format != "" {
		_, _ = fmt.Fprintf(c.out, "  Format:    %s\n", describeSnapshotFormat(result.Format))
	}
	if result.SourceHost != "" {
		_, _ = fmt.Fprintf(c.out, "  Source:    %s (snapshot taken %s)\n", result.SourceHost, result.SnapshotAt)
	}
	if o := result.ArchiveOrigin; o != nil {
		switch o.Kind {
		case "catalogue-downloaded":
			_, _ = fmt.Fprintf(c.out, "  Archive:   downloaded from S3 (%s) to %s — now the snapshot's local copy\n",
				humanSize(o.DownloadedBytes), o.Path)
		case "s3-download":
			_, _ = fmt.Fprintf(c.out, "  Archive:   downloaded from S3 (%s) to %s\n"+
				"             (kept for further restores; the daemon prunes it after 7 days)\n",
				humanSize(o.DownloadedBytes), o.Path)
		case "s3-cached":
			_, _ = fmt.Fprintf(c.out, "  Archive:   reused previously downloaded copy at %s\n"+
				"             (kept for further restores; the daemon prunes it after 7 days)\n", o.Path)
		}
	}
	if result.FinalStatus == "stopped" {
		// A cold-captured instance comes back the way it was captured. Saying
		// nothing would read as a restore that failed to start the instance.
		_, _ = fmt.Fprintf(c.out,
			"\n  The instance was STOPPED when this snapshot was taken, so it has been\n"+
				"  restored, verified, and left stopped. Start it with: oddk instance start %s\n",
			result.Instance)
	}
	if result.PasswordChanged {
		// Anything holding the previous credential is now broken, so this cannot
		// be left to the documentation.
		_, _ = fmt.Fprintf(c.out,
			"\n⚠️  The postgres password is now the snapshot's. Re-read it with\n"+
				"   'oddk instance get-postgres-password %s' and update anything that stored it.\n",
			result.Instance)
	}

	return nil
}

// snapshotApplyAction rebuilds this host from a snapshot.
//
// Unlike every other CLI command this does NOT talk to the daemon: disaster
// recovery has to work when the daemon cannot start, which is frequently why
// the operator is here. It follows the `oddk auth` precedent of opening the
// data dir directly, and so must run as the data-dir owner.
func (c *Client) snapshotApplyAction(ctx context.Context, cmd *cli.Command) error {
	archivePath := cmd.String("file")
	s3URI := cmd.String("s3-uri")
	masterKeyPath := cmd.String("master-key")
	if (archivePath == "") == (s3URI == "") {
		return fmt.Errorf("exactly one of --file or --s3-uri is required (the snapshot to apply)")
	}
	if s3URI == "" && (cmd.String("region") != "" || cmd.String("endpoint") != "" || cmd.String("aws-profile") != "") {
		return fmt.Errorf("--region, --endpoint and --aws-profile are only meaningful with --s3-uri")
	}
	if masterKeyPath == "" {
		return fmt.Errorf("--master-key is required: without the source host's master.key, the restored clusters would have a postgres password ODDK cannot recover")
	}

	dataDir, err := resolveLocalDataDir(cmd)
	if err != nil {
		return err
	}
	backupDir := cmd.String("backup-dir")
	if backupDir == "" {
		backupDir = filepath.Join(dataDir, "backups")
	}
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}

	// Fetch from S3 before anything else: this process's ambient AWS
	// credential chain (env vars, profile, EC2 instance role) is the authority
	// — a DR host has no daemon, no offsite settings and no catalogue, which is
	// exactly why the download lives here. The archive lands in the managed
	// downloads area, additively; a matching previous download is reused.
	var fetched *operations.FetchResult
	if s3URI != "" {
		fetch, err := operations.FetchSnapshotForApply(ctx, &operations.RemoteSnapshotSpec{
			URI:      s3URI,
			Region:   cmd.String("region"),
			Endpoint: cmd.String("endpoint"),
			Profile:  cmd.String("aws-profile"),
		}, backupDir, c.out)
		if err != nil {
			return err
		}
		fetched = fetch
		archivePath = fetch.Path
		if fetch.Reused {
			_, _ = fmt.Fprintf(c.out, "Reusing previously downloaded archive at %s (size matches S3)\n", fetch.Path)
		} else {
			_, _ = fmt.Fprintf(c.out, "Downloaded %s (%s) to %s\n", s3URI, humanSize(fetch.Size), fetch.Path)
		}
	}

	dockerClient, err := docker.NewClient()
	if err != nil {
		return wrapApplyS3Retry(fmt.Errorf("connect to Docker: %w", err), fetched)
	}

	// Image pulls happen in preflight and can take minutes on a DR host, so
	// render Docker's layer progress rather than appearing to hang.
	pullProgress, closePullProgress := c.dockerProgressWriter()
	defer closePullProgress()

	params := &operations.SnapshotApplyParams{
		ArchivePath:   archivePath,
		MasterKeyPath: masterKeyPath,
		DataDir:       dataDir,
		BackupDir:     backupDir,
		DaemonPort:    cmd.Int("daemon-port"),
		Docker:        dockerClient,
		Progress:      c.out,
		PullProgress:  pullProgress,
	}

	_, _ = fmt.Fprintln(c.out, "Reading snapshot...")
	plan, preflightErr := operations.PreflightSnapshotApply(ctx, params)
	defer plan.Cleanup()

	if plan.Manifest != nil {
		_, _ = fmt.Fprintf(c.out, "  Created:   %s by oddk %s on %s\n",
			plan.Manifest.CreatedAt.Format("2006-01-02 15:04:05 UTC"),
			plan.Manifest.OddkVersion, plan.Manifest.SourceHost)
		_, _ = fmt.Fprintf(c.out, "  Instances: %s\n", describeSnapshotInstances(plan.Manifest))
	}

	closePullProgress()

	_, _ = fmt.Fprintln(c.out, "\nPreflight:")
	for _, check := range plan.Checks {
		glyph := glyphOK
		if !check.OK {
			glyph = glyphBad
		}
		if check.Detail != "" {
			_, _ = fmt.Fprintf(c.out, "  %s %s (%s)\n", glyph, check.Label, check.Detail)
		} else {
			_, _ = fmt.Fprintf(c.out, "  %s %s\n", glyph, check.Label)
		}
	}
	if preflightErr != nil {
		return wrapApplyS3Retry(preflightErr, fetched)
	}

	if !cmd.Bool("yes") {
		_, _ = fmt.Fprintln(c.out, "\nAbout to apply this snapshot to this host.")
		_, _ = fmt.Fprintln(c.out)
		_, _ = fmt.Fprintln(c.out, "  - This REPLACES the entire ODDK deployment here: oddk.db, master.key and all")
		_, _ = fmt.Fprintln(c.out, "    instance data.")
		if len(plan.PulledImages) > 0 {
			_, _ = fmt.Fprintf(c.out, "  - No ODDK state has been modified yet. (%d Docker image(s) were pulled\n    above; that is additive and safe to leave behind.)\n", len(plan.PulledImages))
		} else {
			_, _ = fmt.Fprintln(c.out, "  - Nothing has been modified yet; everything above was read-only.")
		}
		if fetched != nil {
			_, _ = fmt.Fprintf(c.out, "  - The downloaded archive at %s is additive too;\n    it is kept for retries and pruned after 7 days.\n", fetched.Path)
		}
		_, _ = fmt.Fprintln(c.out)

		confirmed, err := c.cliConfirm("Proceed with applying snapshot to this host? [y/N]: ")
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(c.out, "Cancelled")
			return nil
		}
	}

	_, _ = fmt.Fprintln(c.out, "\nInstalling configuration...")
	result, err := operations.ExecuteSnapshotApply(ctx, plan, c.out)
	if err != nil {
		return wrapApplyS3Retry(err, fetched)
	}

	_, _ = fmt.Fprintln(c.out, "\nSnapshot applied.")
	_, _ = fmt.Fprintf(c.out, "Instances: %d restored, %d configuration-only\n",
		len(result.Restored), len(result.ConfigOnly))
	_, _ = fmt.Fprintln(c.out, "Next: start the daemon (systemctl start oddk), then run 'oddk checklist'")

	if len(result.ConfigOnly) > 0 {
		_, _ = fmt.Fprintf(c.out,
			"\n⚠️  These instances held NO data in the snapshot and were left in 'error' with no\n"+
				"   cluster: %s\n"+
				"   Their configuration is restored; destroy and recreate them, or restore their\n"+
				"   databases from a per-instance backup.\n",
			strings.Join(result.ConfigOnly, ", "))
	}
	if result.BackupsRepointed > 0 || result.BackupsLocalCleared > 0 {
		_, _ = fmt.Fprintf(c.out, "\nBackup catalogue: %d record(s) re-pointed to this host, %d had no local copy here.\n",
			result.BackupsRepointed, result.BackupsLocalCleared)
	}
	if result.BackupsDangling > 0 {
		_, _ = fmt.Fprintf(c.out,
			"\n⚠️  %d backup record(s) now reference neither a local nor an offsite copy and will be\n"+
				"   dropped from the catalogue when the daemon starts. If you intend to copy the old\n"+
				"   host's backup directory across, do it BEFORE starting the daemon and re-run apply,\n"+
				"   or those records are lost (the backup files themselves are untouched).\n",
			result.BackupsDangling)
	}
	if result.SnapshotsRepointed > 0 || result.SnapshotsLocalCleared > 0 {
		_, _ = fmt.Fprintf(c.out, "\nSnapshot catalogue: %d record(s) re-pointed to this host, %d had no local copy here.\n",
			result.SnapshotsRepointed, result.SnapshotsLocalCleared)
	}
	if result.SnapshotsDangling > 0 {
		// Reported for the same reason as the backup count: these are silently
		// wrong otherwise, and the operator is the only one who can fix them by
		// copying the old host's archives across.
		_, _ = fmt.Fprintf(c.out,
			"\n⚠️  %d snapshot record(s) reference neither a local nor an offsite copy. The archives\n"+
				"   are not on this host. Copy the old host's backup directory across and re-run apply,\n"+
				"   or those snapshots are unreachable (the records remain, pointing at nothing).\n",
			result.SnapshotsDangling)
	}
	if result.TokensReplaced {
		_, _ = fmt.Fprintf(c.out,
			"\n⚠️  Auth tokens were replaced by the source host's. Any token minted on THIS host\n"+
				"   no longer works; the source host's existing ~/.config/oddk/cli.json does.\n")
	}
	if result.ReplacedKeyAs != "" {
		_, _ = fmt.Fprintf(c.out, "\nPrevious master.key saved as %s\n", filepath.Base(result.ReplacedKeyAs))
	}
	if result.ReplacedDBAs != "" {
		_, _ = fmt.Fprintf(c.out, "Previous oddk.db saved as %s\n", filepath.Base(result.ReplacedDBAs))
	}

	return nil
}

// wrapApplyS3Retry appends the retry story to an apply failure when the
// archive was fetched from S3: the download survives the failure, so the
// retry must not pay for it again — and the operator should know that.
func wrapApplyS3Retry(err error, fetched *operations.FetchResult) error {
	if err == nil || fetched == nil {
		return err
	}
	return fmt.Errorf("%w\n\nThe downloaded archive remains at %s;\nre-running with the same --s3-uri reuses it, or pass it directly with --file", err, fetched.Path)
}

// snapshotListRemoteAction lists snapshot archives in S3 — the BUCKET's truth,
// as opposed to `snapshot list`, which is this host's catalogue.
//
// Zero-arg: the daemon lists its configured offsite bucket (it holds the
// credentials). With an s3:// URI: fully local, using this shell's ambient
// AWS credentials — no daemon and no token, which is exactly what a
// disaster-recovery host has before `snapshot apply`.
func (c *Client) snapshotListRemoteAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() > 1 {
		return fmt.Errorf("usage: oddk snapshot list-remote [s3://bucket[/path]]")
	}

	if cmd.Args().Len() == 0 {
		if cmd.String("region") != "" || cmd.String("endpoint") != "" || cmd.String("aws-profile") != "" || cmd.Bool("all") {
			return fmt.Errorf("--region, --endpoint, --aws-profile and --all apply to the direct s3:// form; without a URI the daemon lists its configured offsite bucket")
		}
		resp, err := c.request("GET", "/api/snapshots/remote", nil)
		if err != nil {
			return err
		}
		if cmd.Bool("json") {
			_, _ = fmt.Fprintf(c.out, "%s\n", resp)
			return nil
		}
		var listing struct {
			Bucket    string                            `json:"bucket"`
			Prefix    string                            `json:"prefix"`
			Objects   []operations.RemoteSnapshotObject `json:"objects"`
			Truncated bool                              `json:"truncated"`
		}
		if err := json.Unmarshal(resp, &listing); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
		c.printRemoteSnapshots(listing.Bucket, listing.Prefix, listing.Objects, listing.Truncated)
		return nil
	}

	uri := cmd.Args().First()
	bucket, prefix, err := s3service.ParseS3BucketURI(uri)
	if err != nil {
		return err
	}
	// ODDK uploads snapshots under <bucketPath>*snapshots*/<date>/<file>; the
	// layout segment is ODDK's own invariant, so append it rather than making
	// every operator remember it. --all lists the given path as-is.
	if !cmd.Bool("all") && !strings.Contains(prefix, operations.SnapshotS3Prefix) {
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		prefix += operations.SnapshotS3Prefix + "/"
	}

	client, err := s3service.NewClientAmbient(ctx, s3service.Target{
		Bucket:   bucket,
		Region:   cmd.String("region"),
		Endpoint: cmd.String("endpoint"),
	}, cmd.String("aws-profile"))
	if err != nil {
		return fmt.Errorf("%w\nSet AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, configure a profile (--aws-profile or AWS_PROFILE), or run on a host with an EC2 instance role", err)
	}

	objects, truncated, err := operations.ListRemoteSnapshots(ctx, client, bucket, prefix)
	if err != nil {
		return err
	}

	if cmd.Bool("json") {
		data, err := json.Marshal(map[string]any{
			"bucket": bucket, "prefix": prefix, "objects": objects, "truncated": truncated,
		})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(c.out, "%s\n", data)
		return nil
	}
	c.printRemoteSnapshots(bucket, prefix, objects, truncated)
	return nil
}

func (c *Client) printRemoteSnapshots(bucket, prefix string, objects []operations.RemoteSnapshotObject, truncated bool) {
	if len(objects) == 0 {
		_, _ = fmt.Fprintf(c.out, "No snapshot objects under s3://%s/%s\n", bucket, prefix)
		return
	}

	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "LAST MODIFIED\tSIZE\tURI")
	for _, o := range objects {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n",
			o.LastModified.UTC().Format("2006-01-02 15:04"), humanSize(o.Size), o.URI)
	}
	_ = w.Flush()
	if truncated {
		_, _ = fmt.Fprintf(c.out, "(listing truncated at %d objects; narrow the path)\n", operations.MaxRemoteSnapshotListing)
	}

	// The listing exists to be acted on; hand over the exact next commands.
	_, _ = fmt.Fprintln(c.out)
	_, _ = fmt.Fprintln(c.out, "Restore one instance:  oddk snapshot restore-instance --instance <name> --s3-uri <uri>")
	_, _ = fmt.Fprintln(c.out, "Rebuild a whole host:  oddk snapshot apply --s3-uri <uri> --master-key <path>")
}

// describeSnapshotInstances renders the manifest's instance inventory,
// flagging configuration-only entries up front so the operator sees before
// confirming that some instances carry no data.
func describeSnapshotInstances(manifest *operations.SnapshotManifest) string {
	if len(manifest.Instances) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(manifest.Instances))
	for _, inst := range manifest.Instances {
		if inst.HasData {
			parts = append(parts, fmt.Sprintf("%s (pg%s)", inst.Name, inst.Version))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (pg%s, configuration-only)", inst.Name, inst.Version))
	}
	return strings.Join(parts, ", ")
}

// dockerProgressWriter returns a writer that renders raw Docker JSON progress
// frames to c.out — layer bars on a terminal, plain status lines otherwise —
// and a function that flushes and stops it. Safe to call the closer twice.
//
// The daemon-backed commands get this rendering from streamProgress over HTTP;
// 'snapshot apply' runs daemon-less, so it renders the frames itself.
func (c *Client) dockerProgressWriter() (io.Writer, func()) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fd, isTerm := term.GetFdInfo(c.out)
		_ = jsonmessage.DisplayJSONMessagesStream(pr, c.out, fd, isTerm, nil)
	}()

	var once sync.Once
	return pw, func() {
		once.Do(func() {
			_ = pw.Close()
			<-done
		})
	}
}
