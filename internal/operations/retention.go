package operations

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	s3service "github.com/andrianbdn/oddk/internal/services/s3"
	snapshotstore "github.com/andrianbdn/oddk/internal/store/snapshot"
)

// retentionFloor implements the newest-N rule that age-based retention may never
// cross, for both backups and snapshots.
//
// The rule exists because retention runs even on a night the capture failed —
// deliberately, so a broken job does not also stop pruning. With only an age
// rule, a schedule that has been failing for longer than its cleanup window
// expires EVERY archive and leaves nothing to restore from, precisely when the
// deployment is least able to make a new one.
//
// It counts SURVIVING COPIES, not catalogue rows. That distinction is the whole
// point: a row whose archive was deleted out of band (a cleaned-up disk, an
// unmounted volume, an object removed straight from the bucket) protects
// nothing, and letting it consume a slot means the floor guards ghosts while
// retention deletes the last archive that actually exists — the exact loss the
// floor was added to prevent. The catalogue already models this rule elsewhere:
// `oddk checklist` counts a local copy only when the file is really there.
type retentionFloor struct {
	limit int
	kept  int
}

func newRetentionFloor(limit int) *retentionFloor {
	return &retentionFloor{limit: limit}
}

// protects reports whether the floor holds this record, and consumes a slot when
// it does. copyPresent must describe the copy being aged out (the local file for
// local retention, the S3 object for offsite retention).
func (f *retentionFloor) protects(copyPresent bool) bool {
	if !copyPresent || f.full() {
		return false
	}
	f.kept++
	return true
}

// full reports whether the floor has already found its quota of surviving
// copies. Offsite retention checks it before spending a HeadObject on
// existence, since once the floor is full the answer cannot change the outcome.
func (f *retentionFloor) full() bool {
	return f.kept >= f.limit
}

// snapshotIsComplete reports whether the archive holds data for every instance
// it recorded. Pre-019 rows (Instances == nil, unknown) are treated as complete
// so retention stays conservative rather than aging out the last restorable
// copy because we cannot read the coverage list.
func snapshotIsComplete(rec *snapshotstore.Record) bool {
	if rec.Instances == nil {
		return true
	}
	for _, inst := range rec.Instances {
		if !inst.HasData {
			return false
		}
	}
	return true
}

// retentionKeep says why a snapshot copy survived age-based retention, so the
// two cleanup loops can log the actual reason instead of guessing at it.
type retentionKeep int

const (
	retentionKeepNone retentionKeep = iota
	retentionKeepNewest
	retentionKeepNewestComplete
)

func (k retentionKeep) reason(limit int) string {
	switch k {
	case retentionKeepNewest:
		return fmt.Sprintf("it is one of the newest %d, and expiring every archive would leave nothing to restore from", limit)
	case retentionKeepNewestComplete:
		return "it is the newest complete archive (every instance has data), and degraded copies must not age that out"
	default:
		return "it is not protected"
	}
}

// snapshotRetentionProtects is the keep rule for one snapshot copy: the newest-N
// surviving copies, PLUS the newest complete archive even if it is older.
// Incomplete (degraded) archives still fill newest-N so they remain the latest
// restore point for instances they did capture, but they must not evict the
// last fully restorable copy.
//
// Both rules are always evaluated, never short-circuited: a complete archive
// that newest-N already protects must still consume the complete slot, or the
// next complete archive down would be pinned as well.
func snapshotRetentionProtects(floor *retentionFloor, completeKept *bool, present, isComplete bool) retentionKeep {
	newest := floor.protects(present)
	complete := false
	if present && isComplete && !*completeKept {
		complete = true
		*completeKept = true
	}
	switch {
	case newest:
		return retentionKeepNewest
	case complete:
		return retentionKeepNewestComplete
	default:
		return retentionKeepNone
	}
}

// localArchivePresent reports whether a catalogued local archive is really on
// disk. dataDir resolves the relative locations older backup records may carry;
// pass "" when the location is always absolute.
//
// It FAILS SAFE: only a definite "not there" counts as absent. Any other stat
// error (a permission problem, EIO) reports present, because retention must
// never become MORE willing to delete because the filesystem is sick — the same
// reasoning as the fail-safe offsite lookup that guards the only-copy rule.
func localArchivePresent(dataDir, location string) bool {
	if location == "" {
		return false
	}
	path := location
	if !filepath.IsAbs(path) && dataDir != "" {
		path = filepath.Join(dataDir, path)
	}
	_, err := os.Stat(path)
	return err == nil || !os.IsNotExist(err)
}

// remoteArchivePresent reports whether a catalogued S3 object is really in the
// bucket. Errors report present, for the same fail-safe reason as above: a
// HeadObject that failed on a network blip must not license deleting the
// remaining copies.
//
// Call it only while the floor is still filling. It is one HeadObject per call,
// so bounding it to the newest few records keeps offsite retention at a couple
// of extra requests per run instead of one per catalogued archive.
func remoteArchivePresent(ctx context.Context, client *s3service.Client, configuredBucket, location string) bool {
	bucket, key, err := parseS3Location(location)
	if err != nil {
		// An unparseable location is left to the deletion path, which reports it
		// properly; treating it as absent here would only unprotect the floor.
		return true
	}
	if bucket != configuredBucket {
		// Not ours to check or delete. The deletion path skips it with a warning.
		return true
	}
	exists, err := client.FileExists(ctx, client.RelativeKey(key))
	if err != nil {
		log.Printf("Warning: could not check whether %s still exists (%v); counting it as present so retention stays conservative", location, err)
		return true
	}
	return exists
}
