package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// backupNoticeExempt are the backup subcommands that deliberately print no
// deprecation notice. dangerously-drop-all is how the retirement is FINISHED.
var backupNoticeExempt = map[string]bool{"dangerously-drop-all": true}

// Every backup subcommand must either name its replacement or be exempt on
// purpose — a subcommand added later must not skip the notice by accident.
func TestEveryBackupSubcommandIsCoveredByTheDeprecation(t *testing.T) {
	backup := withBackupDeprecationNotices(backupCommands(&Client{}))
	for _, sub := range backup.Commands {
		_, hasReplacement := backupReplacements[sub.Name]
		switch {
		case backupNoticeExempt[sub.Name]:
			if hasReplacement || sub.Before != nil {
				t.Errorf("%s is exempt but still prints a notice", sub.Name)
			}
		case !hasReplacement:
			t.Errorf("backup %s has no replacement in backupReplacements and is not exempt", sub.Name)
		case sub.Before == nil:
			t.Errorf("backup %s has a replacement but no notice hook", sub.Name)
		}
	}
}

func TestBackupDeprecationNoticeNamesReplacementAndDate(t *testing.T) {
	var buf bytes.Buffer
	old := backupNoticeOut
	backupNoticeOut = &buf
	t.Cleanup(func() { backupNoticeOut = old })

	backup := withBackupDeprecationNotices(backupCommands(&Client{}))
	for _, sub := range backup.Commands {
		if sub.Name != "restore" {
			continue
		}
		if _, err := sub.Before(context.Background(), sub); err != nil {
			t.Fatal(err)
		}
	}
	got := buf.String()
	for _, want := range []string{"'oddk backup restore' is deprecated", "2026-12-31", "oddk snapshot restore-database"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice %q does not contain %q", got, want)
		}
	}
}
