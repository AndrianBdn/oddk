package cli

import (
	"path/filepath"
	"testing"
)

// The daemon and the daemon-less commands (`snapshot apply`) must resolve the
// SAME backup directory. They used to each spell their own default, and the two
// spellings disagreed: the daemon used $HOME/backups while apply used
// <data-dir>/backups.
//
// Under the installed FHS layout that is not a cosmetic difference. install.sh
// runs the daemon with --data-dir /var/lib/oddk/data --backup-dir
// /var/lib/oddk/backups, so apply's default resolved to
// /var/lib/oddk/data/backups: a directory the daemon never uses. A DR host's
// downloaded archive (potentially many GB) landed in its downloads/ area, which
// the daemon's 7-day sweep — bounded to its own backup dir — can never reap, and
// apply reconciled the restored backup catalogue against the wrong directory.
func TestOddkUserBackupDir_IsHomeSiblingNotDataDirChild(t *testing.T) {
	const home = "/var/lib/oddk"
	dataDir := filepath.Join(home, "data") // what resolveLocalDataDir defaults to

	got := oddkUserBackupDir(home)

	if want := "/var/lib/oddk/backups"; got != want {
		t.Errorf("oddkUserBackupDir(%q) = %q, want %q — this is the path install.sh passes to the daemon", home, got, want)
	}
	if got == filepath.Join(dataDir, "backups") {
		t.Errorf("backup dir resolved under the data dir (%q); the daemon uses the home-relative sibling, and a mismatch strands DR downloads where the sweep cannot see them", got)
	}
}

// The legacy /home/oddk layout must resolve the same way, since oddk-update.sh
// leaves those units untouched and they also pass <home>/backups.
func TestOddkUserBackupDir_LegacyHomeLayout(t *testing.T) {
	if got, want := oddkUserBackupDir("/home/oddk"), "/home/oddk/backups"; got != want {
		t.Errorf("oddkUserBackupDir = %q, want %q", got, want)
	}
}
