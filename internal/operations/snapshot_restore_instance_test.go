package operations

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/operr"
)

// TestSnapshotInstancePassword covers the invariant the whole restore rests on:
// the cluster must be initialised with the SOURCE's plaintext postgres password,
// because globals.sql carries only its hash. Recovering it from the snapshot's
// embedded oddk.db is the only way to get it, and the wrong key must refuse
// rather than hand back garbage that would brick the rebuilt instance.
func TestSnapshotInstancePassword(t *testing.T) {
	st, dir := newTestStore(t)
	dbPath := filepath.Join(dir, "oddk.db")

	rightKey := make([]byte, 32)
	for i := range rightKey {
		rightKey[i] = byte(i)
	}
	wrongKey := make([]byte, 32)
	for i := range wrongKey {
		wrongKey[i] = byte(255 - i)
	}

	const plaintext = "s3cret-source-password"
	encrypted, err := crypto.EncryptPassword(plaintext, rightKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Instances.Create("app", 5432, "17", encrypted, "", 1, 512, "default", "postgres:17"); err != nil {
		t.Fatal(err)
	}
	if err := st.Sqlx.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := snapshotInstancePassword(dbPath, "app", rightKey)
	if err != nil {
		t.Fatalf("snapshotInstancePassword with the right key: %v", err)
	}
	if got != plaintext {
		t.Errorf("recovered password = %q, want %q", got, plaintext)
	}

	// The wrong key must be refused. Silently proceeding would create a cluster
	// whose postgres role ends up holding a hash nobody knows the plaintext of.
	if _, err := snapshotInstancePassword(dbPath, "app", wrongKey); err == nil {
		t.Error("wrong master key was accepted; it must refuse")
	}

	// An instance that is not in the embedded store must be reported as such,
	// not treated as "no password".
	if _, err := snapshotInstancePassword(dbPath, "absent", rightKey); err == nil {
		t.Error("missing instance was accepted")
	}
}

func TestSnapshotEntryLookup(t *testing.T) {
	manifest := &SnapshotManifest{
		Instances: []SnapshotInstanceEntry{
			{Name: "app", HasData: true},
			{Name: "reporting", HasData: false, SkipReason: "instance was stopped"},
		},
	}

	if entry, ok := snapshotEntry(manifest, "app"); !ok || !entry.HasData {
		t.Errorf("snapshotEntry(app) = (%+v, %v), want a data-bearing entry", entry, ok)
	}
	if entry, ok := snapshotEntry(manifest, "reporting"); !ok || entry.HasData {
		t.Errorf("snapshotEntry(reporting) = (%+v, %v), want a config-only entry", entry, ok)
	}
	if _, ok := snapshotEntry(manifest, "nope"); ok {
		t.Error("snapshotEntry found an instance that is not in the manifest")
	}

	// The name list is what an operator sees after a typo, so it must list every
	// instance the archive holds - including config-only ones, which are in the
	// archive even though they cannot be restored.
	if got, want := instanceNameList(manifest), "app, reporting"; got != want {
		t.Errorf("instanceNameList = %q, want %q", got, want)
	}
	if got, want := instanceNameList(&SnapshotManifest{}), "none"; got != want {
		t.Errorf("instanceNameList(empty) = %q, want %q", got, want)
	}

	if got := entrySkipReason(manifest.Instances[1]); got != "instance was stopped" {
		t.Errorf("entrySkipReason = %q", got)
	}
	if got := entrySkipReason(SnapshotInstanceEntry{}); got == "" {
		t.Error("entrySkipReason must never be empty; it lands mid-sentence in a refusal")
	}
}

// TestInstanceGetSignalsAbsenceAsError pins the store contract that
// RestoreInstanceFromSnapshot's create-vs-replace branch depends on.
//
// InstanceStore.Get reports "no such instance" as an operr.ErrNotFound ERROR,
// not as (nil, nil). Treating any error as fatal made the create path dead code
// - restoring an instance that does not exist locally, which is exactly what you
// do after destroying a broken one, always failed. If this contract ever changes
// to (nil, nil), this test fails and the branch must be revisited.
func TestInstanceGetSignalsAbsenceAsError(t *testing.T) {
	st, _ := newTestStore(t)

	inst, err := st.Instances.Get("definitely-absent")
	if inst != nil {
		t.Errorf("Get(absent) returned an instance: %+v", inst)
	}
	if err == nil {
		t.Fatal("Get(absent) returned a nil error; the create-vs-replace branch assumes an ErrNotFound error")
	}
	if !errors.Is(err, operr.ErrNotFound) {
		t.Fatalf("Get(absent) error = %v, want one matching operr.ErrNotFound", err)
	}
}
