package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/store"
)

// newTestStore opens a fully migrated store rooted in a fresh temp data dir,
// creating its backups/ subdirectory, and returns the store together with that
// backup dir. The connection is closed on test cleanup.
func newTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dataDir := t.TempDir()
	backupDir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewStore(filepath.Join(dataDir, "oddk.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Sqlx.Close() })
	return st, backupDir
}
