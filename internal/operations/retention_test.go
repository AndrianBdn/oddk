package operations

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The regression this whole file exists for: the floor used to count catalogue
// ROWS. Two rows whose archives had been deleted out of band consumed both
// slots, and retention then deleted the newest archive that actually existed —
// the precise loss the floor was added to prevent, caused by the floor.
func TestRetentionFloor_GhostRecordsDoNotConsumeSlots(t *testing.T) {
	floor := newRetentionFloor(2)

	// Newest-first, as every List/ListBackups returns: two records whose files
	// are gone, then three real archives.
	present := []bool{false, false, true, true, true}
	var protected []int
	for i, p := range present {
		if floor.protects(p) {
			protected = append(protected, i)
		}
	}

	if len(protected) != 2 || protected[0] != 2 || protected[1] != 3 {
		t.Fatalf("floor protected records %v, want the two newest SURVIVING ones (2 and 3)", protected)
	}
	if !floor.full() {
		t.Error("floor reports itself unfilled after protecting its quota")
	}
}

func TestRetentionFloor_ProtectsExactlyTheNewestN(t *testing.T) {
	floor := newRetentionFloor(2)

	for i, want := range []bool{true, true, false, false} {
		if got := floor.protects(true); got != want {
			t.Errorf("record %d: protects = %v, want %v", i, got, want)
		}
	}
}

// Once the quota is met the answer cannot change, so offsite retention skips the
// HeadObject entirely — that is what keeps this to a couple of requests per run.
func TestRetentionFloor_FullShortCircuitsExistenceChecks(t *testing.T) {
	floor := newRetentionFloor(1)
	if floor.full() {
		t.Fatal("an empty floor reports itself full")
	}
	floor.protects(true)
	if !floor.full() {
		t.Error("floor with its quota met does not report full")
	}
}

func TestLocalArchivePresent(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "snapshot.tar.zst")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		dataDir  string
		location string
		want     bool
	}{
		{"absolute path that exists", "", real, true},
		{"absolute path that does not", "", filepath.Join(dir, "gone.tar.zst"), false},
		{"relative path resolved against the data dir", dir, "snapshot.tar.zst", true},
		{"relative path that does not exist", dir, "gone.tar.zst", false},
		{"no location at all", "", "", false},
		// FAILS SAFE: a stat that fails for any reason other than "not there"
		// must report present. ENOTDIR (a path component that is a file) is the
		// deterministic stand-in for the real cases — EACCES, EIO, a sick mount.
		// Retention must never become more willing to delete because the
		// filesystem is broken.
		{"stat fails for a reason other than absence", "", filepath.Join(real, "nested"), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := localArchivePresent(tc.dataDir, tc.location); got != tc.want {
				t.Errorf("localArchivePresent(%q, %q) = %v, want %v", tc.dataDir, tc.location, got, tc.want)
			}
		})
	}
}

// Both of these return before the S3 client is touched (hence the nil), and both
// must report present: neither is a record whose object we know to be gone, and
// unprotecting the floor on a location we cannot even evaluate would be the
// aggressive direction.
func TestRemoteArchivePresent_UnevaluableLocationsCountAsPresent(t *testing.T) {
	ctx := context.Background()

	if !remoteArchivePresent(ctx, nil, "bucket", "/var/lib/oddk/backups/snap.tar.zst") {
		t.Error("an unparseable remote location was treated as absent")
	}
	if !remoteArchivePresent(ctx, nil, "bucket", "s3://someone-elses-bucket/key.tar.zst") {
		t.Error("an archive in another bucket was treated as absent; it is not ours to check or delete")
	}
}
