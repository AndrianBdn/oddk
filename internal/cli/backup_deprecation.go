package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/andrianbdn/oddk/internal/operations"
)

// backupReplacements maps each `oddk backup` subcommand to what replaces it.
// A subcommand missing from the map prints no notice: dangerously-drop-all is
// the tool for FINISHING the retirement, and nagging an operator for using it
// would be backwards.
var backupReplacements = map[string]string{
	"make":          "oddk snapshot make",
	"list":          "oddk snapshot list",
	"upload":        "oddk snapshot upload <id>",
	"download":      "oddk snapshot download <id>",
	"remove-local":  "oddk snapshot remove-local <id>",
	"remove-remote": "oddk snapshot remove-remote <id>",
	"restore":       "oddk snapshot restore-database",
	"setup-cron":    "oddk snapshot setup-cron (move existing schedules with 'oddk snapshot migrate-from-backups')",
	"list-cron":     "oddk snapshot list-cron",
}

// backupNoticeOut is where the notice goes; a variable so tests can capture it.
var backupNoticeOut io.Writer = os.Stderr

// withBackupDeprecationNotices makes every `oddk backup ...` subcommand print a
// deprecation notice naming its replacement before it runs.
//
// The notice goes to STDERR: stdout is what scripts and --json consumers
// parse, and a deprecation must not break the automation it is asking people
// to move off. It is printed on every run, not once per host — there is no
// state to remember it in on the client, and the cutoff is fixed.
func withBackupDeprecationNotices(backup *cli.Command) *cli.Command {
	for _, sub := range backup.Commands {
		replacement, ok := backupReplacements[sub.Name]
		if !ok {
			continue
		}
		previous := sub.Before
		sub.Before = func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			printBackupDeprecation(backupNoticeOut, cmd.Name, replacement)
			if previous != nil {
				return previous(ctx, cmd)
			}
			return ctx, nil
		}
	}
	return backup
}

func printBackupDeprecation(w io.Writer, subcommand, replacement string) {
	_, _ = fmt.Fprintf(w,
		"Notice: 'oddk backup %s' is deprecated; per-instance backups will be removed in the first release after %s.\n"+
			"        Use: %s\n",
		subcommand, operations.LegacyBackupRemovalAfter, replacement)
}
