package operations

import (
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/store"
)

// newTestStore opens a fully migrated store rooted in a fresh temp dir and
// returns it together with that dir (the store's data dir, holding oddk.db).
// The connection is closed on test cleanup; Close is idempotent, so tests that
// close explicitly are unaffected.
func newTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.NewStore(filepath.Join(dir, "oddk.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Sqlx.Close() })
	return st, dir
}
