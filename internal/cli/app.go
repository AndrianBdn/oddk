package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"text/tabwriter"

	"github.com/urfave/cli/v3"

	"github.com/hypersequent/oddk/internal/store/parameters"
	"github.com/hypersequent/oddk/internal/version"
)

// BuildApp creates the CLI application with all commands and subcommands
func BuildApp(client *Client) *cli.Command {
	buildInfo := version.GetBuildInfo()

	return &cli.Command{
		Name:    "oddk",
		Usage:   "Opinionated Database Deployment Kit",
		Version: buildInfo.String(),
		Commands: []*cli.Command{
			daemonCommand(),
			authCommand(),
			pullCommand(client),
			createCommand(client),
			listCommand(client),
			instanceCommands(client),
			notifyCommands(client),
			backupCommands(client),
			parametersCommands(client),
			customKVCommands(client),
			offsiteCommands(client),
		},
	}
}

func daemonCommand() *cli.Command {
	return &cli.Command{
		Name:  "daemon",
		Usage: "Run the ODDK daemon",
		// Hidden from help: end users install via the script and run the daemon
		// under the dedicated service user via systemd. A user running `oddk
		// daemon` themselves just collides with that daemon (port busy) and
		// leaves stray data behind. Still runnable for the service/dev.
		Hidden: true,
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "port",
				Value: 5442,
				Usage: "Port to listen on",
			},
			&cli.StringFlag{
				Name:  "data-dir",
				Usage: "Data directory for SQLite database",
			},
			&cli.StringFlag{
				Name:  "backup-dir",
				Usage: "Directory for storing backups",
			},
			&cli.BoolFlag{
				Name:  "allow-remote",
				Usage: "Bind on all interfaces instead of loopback. Token transits cleartext HTTP — prefer SSH tunneling.",
			},
		},
		Action: daemonAction,
	}
}

// dataDirFlag is shared by all `auth` subcommands: they talk to oddk.db
// directly (not the daemon HTTP API), so they need to locate the data dir.
func dataDirFlag() cli.Flag {
	return &cli.StringFlag{
		Name:  "data-dir",
		Usage: "Data directory holding oddk.db (defaults to the oddk user's data dir)",
	}
}

func authCommand() *cli.Command {
	return &cli.Command{
		Name:  "auth",
		Usage: "Manage CLI auth tokens (run as the oddk service user)",
		Description: "These subcommands read oddk.db directly, so run them as the oddk " +
			"service user, e.g.:\n   eval \"$(sudo -u oddk /usr/local/bin/oddk auth mint)\"",
		Commands: []*cli.Command{
			{
				Name:  "mint",
				Usage: "Mint a new CLI auth token",
				Description: "Mints a fresh token and emits shell to install it into the " +
					"current user's ~/.config/oddk/cli.json:\n" +
					"   eval \"$(sudo -u oddk /usr/local/bin/oddk auth mint)\"\n" +
					"Use --json to print the config as JSON instead of shell.",
				Flags: []cli.Flag{
					&cli.IntFlag{
						Name:  "port",
						Value: 5442,
						Usage: "Daemon port to record in the CLI config",
					},
					&cli.BoolFlag{
						Name:  "json",
						Usage: "Print the CLI config as JSON instead of eval-able shell",
					},
					dataDirFlag(),
				},
				Action: authMintAction,
			},
			{
				Name:   "list",
				Usage:  "List stored auth tokens (id, prefix, created)",
				Flags:  []cli.Flag{dataDirFlag()},
				Action: authListAction,
			},
			{
				Name:      "delete",
				Usage:     "Revoke an auth token by id",
				ArgsUsage: "<id>",
				Flags:     []cli.Flag{dataDirFlag()},
				Action:    authDeleteAction,
			},
		},
	}
}

func pullCommand(client *Client) *cli.Command {
	return &cli.Command{
		Name:  "pull",
		Usage: "Pull PostgreSQL Docker image",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "version",
				Usage: "PostgreSQL version to pull (e.g. 17)",
			},
			&cli.StringFlag{
				Name:  "image",
				Usage: "Custom Docker image to pull (e.g. pgvector/pgvector:pg18-trixie)",
			},
		},
		Action: client.pullAction,
	}
}

func createCommand(client *Client) *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "Create a new RDBMS instance",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "name",
				Required: true,
				Usage:    "Instance name",
			},
			&cli.IntFlag{
				Name:     "cpu",
				Usage:    "CPU cores (required)",
				Required: true,
			},
			&cli.StringFlag{
				Name:     "ram",
				Usage:    "RAM in GB (default) or with suffix M/MB/MiB for megabytes",
				Required: true,
			},
			&cli.StringFlag{
				Name:        "version",
				Usage:       "PostgreSQL version",
				DefaultText: "17",
			},
			&cli.IntFlag{
				Name:        "port",
				Usage:       "Port number",
				DefaultText: "5432",
			},
			&cli.StringFlag{
				Name:        "parameter-group",
				Usage:       "Parameter group name",
				DefaultText: parameters.DefaultParameterGroup,
			},
			&cli.StringFlag{
				Name:  "image",
				Usage: "Custom Docker image (e.g. pgvector/pgvector:pg17)",
			},
		},
		Action: client.createAction,
	}
}

func listCommand(client *Client) *cli.Command {
	return &cli.Command{
		Name:   "list",
		Usage:  "List all RDBMS instances",
		Action: client.listAction,
	}
}

func instanceCommands(client *Client) *cli.Command {
	return &cli.Command{
		Name:  "instance",
		Usage: "Manage a specific RDBMS instance",
		Commands: []*cli.Command{
			{
				Name:      "status",
				Usage:     "Get instance status",
				ArgsUsage: "<instance-name>",
				Action:    client.statusAction,
			},
			{
				Name:      "start",
				Usage:     "Start instance",
				ArgsUsage: "<instance-name>",
				Action:    client.startAction,
			},
			{
				Name:      "stop",
				Usage:     "Stop instance",
				ArgsUsage: "<instance-name>",
				Action:    client.stopAction,
			},
			{
				Name:      "destroy",
				Usage:     "Destroy instance and all data",
				ArgsUsage: "<instance-name>",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:    "force",
						Aliases: []string{"f"},
						Usage:   "Skip confirmation prompt",
					},
				},
				Action: client.destroyAction,
			},
			{
				Name:      "list-dbs",
				Usage:     "List databases in instance",
				ArgsUsage: "<instance-name>",
				Action:    client.listDatabasesAction,
			},
			{
				Name:      "create-db",
				Usage:     "Create a new database (optionally with its owner user in one step)",
				ArgsUsage: "<instance-name>",
				Description: "With --username, the database and its owner user are created together\n" +
					"(if database creation fails, the just-created user is rolled back) — the\n" +
					"right setup for deploying a service that runs its own migrations. The\n" +
					"generated password and connection string are printed once.\n\n" +
					"For read-only or additional users, use add-db-user once the database\n" +
					"exists.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "database",
						Required: true,
						Usage:    "Database name to create",
					},
					&cli.StringFlag{
						Name:  "username",
						Usage: "Also create this user as the database owner",
					},
				},
				Action: client.createDatabaseAction,
			},
			{
				Name:      "get-postgres-password",
				Usage:     "Get PostgreSQL password",
				ArgsUsage: "<instance-name>",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:  "plain",
						Usage: "Output password only",
					},
					&cli.BoolFlag{
						Name:  "conn",
						Usage: "Output connection string",
					},
					&cli.BoolFlag{
						Name:  "envs",
						Usage: "Output environment variables",
					},
				},
				Action: client.getPasswordAction,
			},
			{
				Name:      "set-postgres-password",
				Usage:     "Set PostgreSQL password (requires NEW_PGPASSWORD env var)",
				ArgsUsage: "<instance-name>",
				Action:    client.setPasswordAction,
			},
			{
				Name:      "psql",
				Usage:     "Launch interactive PostgreSQL shell",
				ArgsUsage: "<instance-name> [psql-args...]",
				Action:    client.psqlAction,
			},
			{
				Name:      "logs",
				Usage:     "Show container logs for instance",
				ArgsUsage: "<instance-name>",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "tail",
						Value: "100",
						Usage: "Number of lines to show from end of logs (or 'all')",
					},
					&cli.BoolFlag{
						Name:    "follow",
						Aliases: []string{"f"},
						Usage:   "Follow log output",
					},
				},
				Action: client.logsAction,
			},
			{
				Name:      "add-db-user",
				Usage:     "Add database user",
				ArgsUsage: "<instance-name>",
				Description: "Adds a user to an existing database.\n\n" +
					"Tip: when provisioning a new service, create the database and its owner\n" +
					"user in one step instead:\n" +
					"   oddk instance create-db <instance> --database <db> --username <user>",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "username",
						Required: true,
						Usage:    "Username to create",
					},
					&cli.StringFlag{
						Name:     "database",
						Required: true,
						Usage:    "Database name",
					},
					&cli.BoolFlag{
						Name:  "readonly",
						Usage: "Grant read-only access",
					},
					&cli.BoolFlag{
						Name:  "owner",
						Usage: "Make user the owner of the database (for running migrations)",
					},
				},
				Action: client.addDatabaseUserAction,
			},
			{
				Name:      "delete-db-user",
				Usage:     "Delete database user",
				ArgsUsage: "<instance-name>",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "username",
						Required: true,
						Usage:    "Username to delete",
					},
					&cli.BoolFlag{
						Name:    "force",
						Aliases: []string{"f"},
						Usage:   "Skip confirmation prompt",
					},
				},
				Action: client.deleteDatabaseUserAction,
			},
			{
				Name:      "reset-db-user-password",
				Usage:     "Reset database user password",
				ArgsUsage: "<instance-name>",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "username",
						Required: true,
						Usage:    "Username to reset password for",
					},
				},
				Action: client.resetDatabaseUserPasswordAction,
			},
			{
				Name:      "apply",
				Usage:     "Apply a new parameter group to instance",
				ArgsUsage: "<instance-name>",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "parameter-group",
						Required: true,
						Usage:    "Parameter group name to apply",
					},
				},
				Action: client.applyAction,
			},
			{
				Name:      "switch",
				Usage:     "Switch instance to a different Docker image",
				ArgsUsage: "<instance-name>",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "image",
						Required: true,
						Usage:    "New Docker image (e.g. pgvector/pgvector:pg17)",
					},
					&cli.StringFlag{
						Name:  "version",
						Usage: "PostgreSQL major version of the image (must match the instance's current major; use 'major-upgrade' to change majors)",
					},
				},
				Action: client.switchAction,
			},
			{
				Name:      "update",
				Usage:     "Re-pull the instance's image tag and recreate it if a newer patch is available (same major; brief downtime)",
				ArgsUsage: "<instance-name>",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "image",
						Usage: "Override image to pull and switch to (same major; default: the instance's current image)",
					},
				},
				Action: client.updateAction,
			},
			{
				Name:      "major-upgrade",
				Usage:     "Upgrade an instance to a newer PostgreSQL major version (dump/restore; causes downtime)",
				ArgsUsage: "<instance-name>",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "target-version",
						Required: true,
						Usage:    "Target PostgreSQL major version (e.g. 18)",
					},
					&cli.StringFlag{
						Name:  "image",
						Usage: "Target Docker image; required for custom images (e.g. pgvector/pgvector:pg18-trixie)",
					},
					&cli.BoolFlag{
						Name:  "yes",
						Usage: "Skip the confirmation prompt",
					},
				},
				Action: client.majorUpgradeAction,
			},
		},
	}
}

func notifyCommands(client *Client) *cli.Command {
	return &cli.Command{
		Name:  "notify",
		Usage: "Manage notifications",
		Commands: []*cli.Command{
			{
				Name:   "info",
				Usage:  "Show notification configuration overview",
				Action: client.notifyInfoAction,
			},
			{
				Name:   "get",
				Usage:  "Get configuration in JSON (or template if no configuration)",
				Action: client.notifyGetAction,
			},
			{
				Name:  "apply",
				Usage: "Apply notification configuration from JSON file",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "file",
						Required: true,
						Usage:    "JSON configuration file path containing array of notifications",
					},
				},
				Description: "Apply all notification configurations from a JSON array in the file",
				Action:      client.notifyApplyAction,
			},
			{
				Name:  "remove",
				Usage: "Remove all notification configurations",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:    "force",
						Aliases: []string{"f"},
						Usage:   "Skip confirmation prompt",
					},
				},
				Action: client.notifyRemoveAction,
			},
			{
				Name:  "help-add",
				Usage: "Show JSON template for specific notification type",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "type",
						Required: true,
						Usage:    "Notification type (email, slack, telegram, webhook)",
					},
				},
				Description: "Output a JSON template for the specified notification type to stdout",
				Action:      client.notifyHelpAddAction,
			},
			{
				Name:   "test",
				Usage:  "Send test notification",
				Action: client.notifyTestAction,
			},
			{
				Name:  "logs",
				Usage: "View notification logs",
				Flags: []cli.Flag{
					&cli.IntFlag{
						Name:  "limit",
						Value: 50,
						Usage: "Number of log entries to show",
					},
				},
				Action: client.notifyLogsAction,
			},
		},
	}
}

func backupCommands(client *Client) *cli.Command {
	return &cli.Command{
		Name:  "backup",
		Usage: "Manage backups",
		Commands: []*cli.Command{
			{
				Name:      "make",
				Usage:     "Create a backup",
				ArgsUsage: "<instance-name>",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "comment",
						Usage: "Optional comment for the backup",
					},
				},
				Action: client.backupMakeAction,
			},
			{
				Name:  "list",
				Usage: "List all backups",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "instance",
						Usage: "Filter by instance name",
					},
				},
				Action: client.backupListAction,
			},
			{
				Name:      "upload",
				Usage:     "Upload a backup to S3",
				ArgsUsage: "<instance-name> <backup-id>",
				Action:    client.backupUploadAction,
			},
			{
				Name:      "download",
				Usage:     "Download a backup from S3 to local storage",
				ArgsUsage: "<instance-name> <backup-id>",
				Action:    client.backupDownloadAction,
			},
			{
				Name:  "setup-cron",
				Usage: "Setup or remove scheduled daily backup for an instance",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "instance",
						Required: true,
						Usage:    "Instance name",
					},
					&cli.IntFlag{
						Name:  "utc-hour",
						Usage: "Hour in UTC (0-23) when backup should run (required if not removing)",
						Value: 3, // Default to 3 AM UTC
					},
					&cli.IntFlag{
						Name:  "cleanup-local-days",
						Usage: "Number of days to keep local backups",
						Value: 7,
					},
					&cli.IntFlag{
						Name:  "cleanup-remote-days",
						Usage: "Number of days to keep remote backups (S3)",
						Value: 14,
					},
					&cli.BoolFlag{
						Name:  "remove",
						Usage: "Remove scheduled backup for this instance",
					},
				},
				Action: client.backupSetupCronAction,
			},
			{
				Name:   "list-cron",
				Usage:  "List all scheduled backups",
				Action: client.backupListCronAction,
			},
			{
				Name:      "remove-local",
				Usage:     "Remove local copy of a backup",
				ArgsUsage: "<instance-name> <backup-id>",
				Action:    client.backupRemoveLocalAction,
			},
			{
				Name:      "remove-remote",
				Usage:     "Remove remote (S3) copy of a backup",
				ArgsUsage: "<instance-name> <backup-id>",
				Action:    client.backupRemoveRemoteAction,
			},
			{
				Name:  "restore",
				Usage: "Restore a database from a backup",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "id",
						Usage: "Backup ID from backup list (mutually exclusive with --file)",
					},
					&cli.StringFlag{
						Name:  "file",
						Usage: "Path to backup file (.tar.zst) (mutually exclusive with --id)",
					},
					&cli.StringFlag{
						Name:     "instance",
						Required: true,
						Usage:    "Target instance name to restore to",
					},
					&cli.StringFlag{
						Name:     "database",
						Required: true,
						Usage:    "Database name inside the backup to restore",
					},
					&cli.StringFlag{
						Name:  "restore-as",
						Usage: "Restore under a different database name",
					},
				},
				Action: client.backupRestoreAction,
			},
		},
	}
}

func parametersCommands(client *Client) *cli.Command {
	return &cli.Command{
		Name:  "parameters",
		Usage: "Manage parameter groups",
		Commands: []*cli.Command{
			{
				Name:  "get",
				Usage: "Get parameter group(s)",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "name",
						Usage: "Parameter group name (if empty, lists all)",
					},
					&cli.BoolFlag{
						Name:  "json",
						Usage: "Output as JSON",
					},
				},
				Action: client.parametersGetAction,
			},
			{
				Name:      "put",
				Usage:     "Create a new parameter group from JSON",
				ArgsUsage: "<group-name>",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "file",
						Required: true,
						Usage:    "JSON file containing parameters",
					},
				},
				Action: client.parametersPutAction,
			},
			{
				Name:      "delete",
				Usage:     "Delete a parameter group",
				ArgsUsage: "<group-name>",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:    "force",
						Aliases: []string{"f"},
						Usage:   "Skip confirmation prompt",
					},
				},
				Action: client.parametersDeleteAction,
			},
		},
	}
}

func customKVCommands(client *Client) *cli.Command {
	return &cli.Command{
		Name:  "customkv",
		Usage: "Manage custom key-value settings",
		Commands: []*cli.Command{
			{
				Name:   "list",
				Usage:  "List all custom key-value pairs",
				Action: client.customKVListAction,
			},
			{
				Name:      "get",
				Usage:     "Get a custom key-value pair",
				ArgsUsage: "<key>",
				Action:    client.customKVGetAction,
			},
			{
				Name:      "set",
				Usage:     "Set a custom key-value pair",
				ArgsUsage: "<key>",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "value",
						Required: true,
						Usage:    "Value to set for the key",
					},
				},
				Description: "Key must end with .str (for strings) or .int (for integers).\n" +
					"   For .int keys, value must be a valid integer.",
				Action: client.customKVSetAction,
			},
		},
	}
}

func offsiteCommands(client *Client) *cli.Command {
	return &cli.Command{
		Name:  "offsite",
		Usage: "Manage offsite backup configuration (S3)",
		Commands: []*cli.Command{
			{
				Name:   "info",
				Usage:  "Show offsite configuration details (except secret)",
				Action: client.offsiteInfoAction,
			},
			{
				Name:  "logs",
				Usage: "Show offsite upload logs",
				Flags: []cli.Flag{
					&cli.IntFlag{
						Name:  "limit",
						Value: 50,
						Usage: "Number of log entries to show",
					},
				},
				Action: client.offsiteLogsAction,
			},
			{
				Name:   "get",
				Usage:  "Get configuration in JSON (or template if no configuration)",
				Action: client.offsiteGetAction,
			},
			{
				Name:  "apply",
				Usage: "Apply offsite configuration",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "file",
						Required: true,
						Usage:    "JSON configuration file path",
					},
				},
				Description: "If secret_access_key is '%SAME-AS-BEFORE%', uses the previous secret key",
				Action:      client.offsiteApplyAction,
			},
			{
				Name:  "remove",
				Usage: "Remove offsite configuration",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:    "force",
						Aliases: []string{"f"},
						Usage:   "Skip confirmation prompt",
					},
				},
				Action: client.offsiteRemoveAction,
			},
			{
				Name:   "test",
				Usage:  "Test offsite configuration by uploading, downloading, and deleting a test file",
				Action: client.offsiteTestAction,
			},
		},
	}
}

func daemonAction(ctx context.Context, cmd *cli.Command) error {
	port := cmd.Int("port")
	dataDir := cmd.String("data-dir")
	backupDir := cmd.String("backup-dir")
	allowRemote := cmd.Bool("allow-remote")

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("could not determine home directory: %w", err)
	}

	username, err := user.Current()
	if err != nil {
		return fmt.Errorf("failed to get current user name: %w", err)
	}

	if dataDir == "" {
		if username.Username == "oddk" {
			dataDir = filepath.Join(home, "data")
		} else {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get current directory: %w", err)
			}
			dataDir = cwd
		}
	}

	if backupDir == "" {
		if username.Username == "oddk" {
			backupDir = filepath.Join(home, "backups")
		} else {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get current directory: %w", err)
			}
			backupDir = cwd
		}
	}

	return runDaemon(port, dataDir, backupDir, allowRemote)
}

// Helper function to write tabular output
func writeTable(out io.Writer, headers []string, rows [][]string) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	// Write headers
	for i, h := range headers {
		if i > 0 {
			if _, err := fmt.Fprint(w, "\t"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprint(w, h); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	// Write rows
	for _, row := range rows {
		for i, col := range row {
			if i > 0 {
				if _, err := fmt.Fprint(w, "\t"); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprint(w, col); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	return w.Flush()
}
