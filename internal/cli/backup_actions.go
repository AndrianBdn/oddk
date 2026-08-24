package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/urfave/cli/v3"
)

func (c *Client) backupMakeAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() < 1 {
		_ = cli.ShowSubcommandHelp(cmd)
		return fmt.Errorf("instance name required")
	}
	name := cmd.Args().Get(0)
	comment := cmd.String("comment")

	var body any
	if comment != "" {
		body = map[string]string{"comment": comment}
	}

	resp, err := c.request("POST", fmt.Sprintf("/api/rdbms/%s/backup", name), body)
	if err != nil {
		return err
	}

	var result struct {
		BackupPath string `json:"backupPath"`
		Size       int64  `json:"size"`
		Timestamp  string `json:"timestamp"`
		Message    string `json:"message"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
	_, _ = fmt.Fprintf(c.out, "Backup path: %s\n", result.BackupPath)
	_, _ = fmt.Fprintf(c.out, "Size: %d bytes\n", result.Size)
	_, _ = fmt.Fprintf(c.out, "Timestamp: %s\n", result.Timestamp)

	return nil
}

func (c *Client) backupListAction(ctx context.Context, cmd *cli.Command) error {
	instanceFilter := cmd.String("instance")

	// If instance is specified, get backups for that instance
	// Otherwise, get backups for all instances
	var resp []byte
	var err error

	if instanceFilter != "" {
		resp, err = c.request("GET", fmt.Sprintf("/api/rdbms/%s/backups", instanceFilter), nil)
	} else {
		// Get all backups across all instances
		resp, err = c.request("GET", "/api/backups", nil)
	}

	if err != nil {
		return err
	}

	var backups []struct {
		ID             int    `json:"id"`
		InstanceName   string `json:"instanceName"`
		Timestamp      string `json:"timestamp"`
		Size           int64  `json:"size"`
		LocalLocation  string `json:"localLocation,omitempty"`
		RemoteLocation string `json:"remoteLocation,omitempty"`
		Status         string `json:"status"`
		Comment        string `json:"comment,omitempty"`
	}

	if err := json.Unmarshal(resp, &backups); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if len(backups) == 0 {
		if instanceFilter != "" {
			_, _ = fmt.Fprintf(c.out, "No backups found for instance %s\n", instanceFilter)
		} else {
			_, _ = fmt.Fprintf(c.out, "No backups found\n")
		}
		return nil
	}

	// Include INSTANCE column when showing all backups
	headers := []string{"ID", "INSTANCE", "TIMESTAMP", "SIZE", "STATUS", "COMMENT", "LOCATION"}
	var rows [][]string

	for _, backup := range backups {
		comment := backup.Comment
		if comment == "" {
			comment = "-"
		}

		// Show location as "Local", "S3", or "Local+S3"
		var location string
		switch {
		case backup.LocalLocation != "" && backup.RemoteLocation != "":
			location = "Local+S3"
		case backup.LocalLocation != "":
			location = "Local"
		case backup.RemoteLocation != "":
			location = "S3"
		default:
			location = "None"
		}

		rows = append(rows, []string{
			fmt.Sprintf("%d", backup.ID),
			backup.InstanceName,
			backup.Timestamp,
			fmt.Sprintf("%d", backup.Size),
			backup.Status,
			comment,
			location,
		})
	}

	return writeTable(c.out, headers, rows)
}

func (c *Client) backupUploadAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() < 2 {
		_ = cli.ShowSubcommandHelp(cmd)
		return fmt.Errorf("instance name and backup ID required")
	}
	instanceName := cmd.Args().Get(0)
	backupIDStr := cmd.Args().Get(1)

	backupID, err := strconv.Atoi(backupIDStr)
	if err != nil {
		return fmt.Errorf("invalid backup ID: %w", err)
	}

	body, err := json.Marshal(map[string]int{"backupId": backupID})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := c.request("POST", fmt.Sprintf("/api/rdbms/%s/backup/%d/upload", instanceName, backupID), body)
	if err != nil {
		return err
	}

	var result struct {
		Message  string `json:"message"`
		Location string `json:"location"`
		Size     int64  `json:"size"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
	_, _ = fmt.Fprintf(c.out, "S3 Location: %s\n", result.Location)
	_, _ = fmt.Fprintf(c.out, "Size: %d bytes\n", result.Size)

	return nil
}

func (c *Client) backupSetupCronAction(ctx context.Context, cmd *cli.Command) error {
	instanceName := cmd.String("instance")
	remove := cmd.Bool("remove")

	if instanceName == "" {
		_ = cli.ShowSubcommandHelp(cmd)
		return fmt.Errorf("instance name required")
	}

	if cmd.Bool("pause") && cmd.Bool("resume") {
		return fmt.Errorf("--pause and --resume are mutually exclusive")
	}
	if cmd.Bool("pause") || cmd.Bool("resume") {
		verb := "resume"
		if cmd.Bool("pause") {
			verb = "pause"
		}
		if _, err := c.request("POST", fmt.Sprintf("/api/cron/backup/%s/%s", instanceName, verb), map[string]any{}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(c.out, "Backup schedule for instance '%s' %sd\n", instanceName, verb)
		return nil
	}

	if remove {
		_, err := c.request("DELETE", fmt.Sprintf("/api/cron/backup/%s", instanceName), nil)
		if err != nil {
			return err
		}

		_, _ = fmt.Fprintf(c.out, "Removed scheduled backup for instance '%s'\n", instanceName)
		return nil
	}

	// Send only fields the operator set, so moving --utc-hour does not silently
	// reset retention windows the way the flag defaults would.
	req := map[string]any{"instanceName": instanceName}
	if cmd.IsSet("utc-hour") {
		utcHour := cmd.Int("utc-hour")
		if utcHour < 0 || utcHour > 23 {
			return fmt.Errorf("UTC hour must be between 0 and 23")
		}
		req["utcHour"] = utcHour
	}
	if cmd.IsSet("cleanup-local-days") {
		n := cmd.Int("cleanup-local-days")
		if n < 1 {
			return fmt.Errorf("cleanup-local-days must be at least 1")
		}
		req["cleanupLocalDays"] = n
	}
	if cmd.IsSet("cleanup-remote-days") {
		n := cmd.Int("cleanup-remote-days")
		if n < 1 {
			return fmt.Errorf("cleanup-remote-days must be at least 1")
		}
		req["cleanupRemoteDays"] = n
	}

	body, err := c.request("POST", "/api/cron/backup", req)
	if err != nil {
		return err
	}

	var plan struct {
		UTCHour           int `json:"utcHour"`
		CleanupLocalDays  int `json:"cleanupLocalDays"`
		CleanupRemoteDays int `json:"cleanupRemoteDays"`
	}
	if err := json.Unmarshal(body, &plan); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "Scheduled daily backup for instance '%s' at %02d:00 UTC (keep local: %d days, remote: %d days)\n",
		instanceName, plan.UTCHour, plan.CleanupLocalDays, plan.CleanupRemoteDays)
	return nil
}

func (c *Client) backupListCronAction(ctx context.Context, cmd *cli.Command) error {
	resp, err := c.request("GET", "/api/cron/backup", nil)
	if err != nil {
		return err
	}

	var plans []struct {
		InstanceName      string `json:"instanceName"`
		UTCHour           int    `json:"utcHour"`
		CleanupLocalDays  int    `json:"cleanupLocalDays"`
		CleanupRemoteDays int    `json:"cleanupRemoteDays"`
		PausedAt          string `json:"pausedAt"`
		PausedReason      string `json:"pausedReason"`
		CreatedAt         string `json:"createdAt"`
		UpdatedAt         string `json:"updatedAt"`
	}

	if err := json.Unmarshal(resp, &plans); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	if len(plans) == 0 {
		_, _ = fmt.Fprintf(c.out, "No scheduled backups configured\n")
		return nil
	}

	// STATUS is not decoration. A paused plan takes NO backups, and 'snapshot
	// apply' pauses every restored one, so a listing that showed only the hour
	// would describe protection that is not running — on a DR host, for the
	// whole time nobody notices. Same rule as 'snapshot list-cron' and the
	// checklist.
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "INSTANCE\tUTC HOUR\tSCHEDULE\tKEEP LOCAL\tKEEP REMOTE\tSTATUS\tUPDATED")
	for _, plan := range plans {
		status := "active"
		if plan.PausedAt != "" {
			status = "PAUSED"
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%02d:00 UTC\t%d days\t%d days\t%s\t%s\n",
			plan.InstanceName,
			plan.UTCHour,
			plan.UTCHour,
			plan.CleanupLocalDays,
			plan.CleanupRemoteDays,
			status,
			plan.UpdatedAt,
		)
	}
	_ = w.Flush()

	for _, plan := range plans {
		if plan.PausedAt == "" {
			continue
		}
		_, _ = fmt.Fprintf(c.out, "\n⚠️  %s is PAUSED since %s — no backups are being taken.\n",
			plan.InstanceName, plan.PausedAt)
		if plan.PausedReason != "" {
			_, _ = fmt.Fprintf(c.out, "    reason: %s\n", plan.PausedReason)
		}
		_, _ = fmt.Fprintf(c.out, "    resume with: oddk backup setup-cron --instance %s --resume\n", plan.InstanceName)
	}

	return nil
}

func (c *Client) backupRemoveLocalAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() < 2 {
		_ = cli.ShowSubcommandHelp(cmd)
		return fmt.Errorf("instance name and backup ID required")
	}
	instanceName := cmd.Args().Get(0)
	backupIDStr := cmd.Args().Get(1)

	backupID, err := strconv.Atoi(backupIDStr)
	if err != nil {
		return fmt.Errorf("invalid backup ID: %w", err)
	}

	resp, err := c.request("DELETE", fmt.Sprintf("/api/rdbms/%s/backup/%d/local", instanceName, backupID), nil)
	if err != nil {
		return err
	}

	var result struct {
		Message string `json:"message"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
	return nil
}

func (c *Client) backupRemoveRemoteAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() < 2 {
		_ = cli.ShowSubcommandHelp(cmd)
		return fmt.Errorf("instance name and backup ID required")
	}
	instanceName := cmd.Args().Get(0)
	backupIDStr := cmd.Args().Get(1)

	backupID, err := strconv.Atoi(backupIDStr)
	if err != nil {
		return fmt.Errorf("invalid backup ID: %w", err)
	}

	resp, err := c.request("DELETE", fmt.Sprintf("/api/rdbms/%s/backup/%d/remote", instanceName, backupID), nil)
	if err != nil {
		return err
	}

	var result struct {
		Message string `json:"message"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
	return nil
}

func (c *Client) backupDownloadAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() < 2 {
		_ = cli.ShowSubcommandHelp(cmd)
		return fmt.Errorf("instance name and backup ID required")
	}
	instanceName := cmd.Args().Get(0)
	backupIDStr := cmd.Args().Get(1)

	backupID, err := strconv.Atoi(backupIDStr)
	if err != nil {
		return fmt.Errorf("invalid backup ID: %w", err)
	}

	resp, err := c.request("POST", fmt.Sprintf("/api/rdbms/%s/backup/%d/download", instanceName, backupID), nil)
	if err != nil {
		return err
	}

	var result struct {
		Message  string `json:"message"`
		Location string `json:"location"`
		Size     int64  `json:"size"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
	_, _ = fmt.Fprintf(c.out, "Local path: %s\n", result.Location)
	_, _ = fmt.Fprintf(c.out, "Size: %d bytes\n", result.Size)
	return nil
}

func (c *Client) backupRestoreAction(ctx context.Context, cmd *cli.Command) error {
	// Get required flags
	instanceName := cmd.String("instance")
	databaseName := cmd.String("database")

	if instanceName == "" {
		_ = cli.ShowSubcommandHelp(cmd)
		return fmt.Errorf("--instance is required")
	}
	if databaseName == "" {
		_ = cli.ShowSubcommandHelp(cmd)
		return fmt.Errorf("--database is required")
	}

	// Get mutually exclusive flags
	backupIDStr := cmd.String("id")
	filePath := cmd.String("file")

	if backupIDStr == "" && filePath == "" {
		_ = cli.ShowSubcommandHelp(cmd)
		return fmt.Errorf("either --id or --file must be provided")
	}
	if backupIDStr != "" && filePath != "" {
		return fmt.Errorf("--id and --file are mutually exclusive")
	}

	// Optional flag
	restoreAs := cmd.String("restore-as")

	// Build request body
	body := map[string]any{
		"databaseName": databaseName,
	}
	if backupIDStr != "" {
		backupID, err := strconv.Atoi(backupIDStr)
		if err != nil {
			return fmt.Errorf("invalid backup ID: %w", err)
		}
		body["backupId"] = backupID
	}
	if filePath != "" {
		body["filePath"] = filePath
	}
	if restoreAs != "" {
		body["restoreAs"] = restoreAs
	}

	// Make request
	resp, err := c.request("POST", fmt.Sprintf("/api/rdbms/%s/restore", instanceName), body)
	if err != nil {
		return err
	}

	var result struct {
		TargetDatabase string `json:"targetDatabase"`
		SourceBackup   string `json:"sourceBackup"`
		Message        string `json:"message"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	_, _ = fmt.Fprintf(c.out, "%s\n", result.Message)
	_, _ = fmt.Fprintf(c.out, "Target database: %s\n", result.TargetDatabase)
	_, _ = fmt.Fprintf(c.out, "Source: %s\n", result.SourceBackup)

	return nil
}
