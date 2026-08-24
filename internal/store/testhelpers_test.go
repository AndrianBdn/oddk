package store_test

import (
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/store"
)

// newTestMasterKey mints a real master.key in dir, exactly as the daemon does.
// NewStore requires one because the notification store encrypts with it.
func newTestMasterKey(t *testing.T, dir string) []byte {
	t.Helper()
	key, err := crypto.GetOrCreateKeyFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// newTestStore opens a fully migrated store in a fresh temp dir; the
// connection is closed on test cleanup.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, _ := newTestStoreDir(t)
	return st
}

// newTestStoreDir is newTestStore for tests that also need the data dir —
// which doubles as the directory holding oddk.db — e.g. to stage files next to
// the store or to reopen the same database file. Close is idempotent, so tests
// that close explicitly before reopening are unaffected by the cleanup.
func newTestStoreDir(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.NewStore(filepath.Join(dir, "oddk.db"), dir, newTestMasterKey(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Sqlx.Close() })
	return st, dir
}
