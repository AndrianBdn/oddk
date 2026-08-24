package operations

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

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
		return fmt.Sprintf("it is the newest complete archive (every instance has data), and degraded copies must not age that out within %d days of the retention window", completePinGraceDays)
	default:
		return "it is not protected"
	}
}

// completePinGraceDays bounds how far past the retention window the
// newest-complete-archive pin may hold an archive.
//
// The pin exists because a degraded capture — one where some instance was
// reduced to configuration-only — must not evict the last archive that holds
// everybody's data. That reasoning holds while the degradation is TRANSIENT: an
// instance somebody restarts, a base backup that failed on a full disk. It stops
// holding when the degradation is PERMANENT. An instance that has been
// configuration-only for a month is a deployment the operator has decided to run
// that way, and an unbounded pin is then not protection but an archive that can
// never expire, on a disk and in a bucket that are still being paid for.
//
// Worse, it is a SILENT fossil. The pinned copy ages without limit while
// `oddk checklist` reports that instance as not covered, so a deployment that
// looks protected is really one stale archive plus a standing warning nobody has
// connected to it.
//
// The grace period is ADDED to each tier's own retention window rather than
// multiplying it, so the extra cost is a constant an operator can reason about —
// one archive, at most 30 days past whatever they configured — instead of one
// that scales with a long window. A 365-day offsite policy would otherwise pin a
// second year of storage.
//
// At a daily schedule, 30 days is 30 consecutive degraded captures, every one of
// which already failed its cron run and sent a notification. Nothing is dropped
// here that was not preceded by a month of ignored alerts.
const completePinGraceDays = 30

// completePinCutoff is the age boundary past which the complete-archive pin
// releases: the tier's own retention window plus the grace period above.
func completePinCutoff(now time.Time, cleanupDays int) time.Time {
	return now.AddDate(0, 0, -(cleanupDays + completePinGraceDays))
}

// retentionVerdict is the keep/delete decision for one snapshot copy, plus the
// one fact the caller cannot recompute for itself: that this copy WAS the newest
// complete archive and is being deleted only because the pin's grace period ran
// out. That deletion is the moment a deployment stops holding any fully
// restorable archive, so it has to be reported rather than merely done.
type retentionVerdict struct {
	keep       retentionKeep
	pinExpired bool
}

// snapshotRetentionProtects is the keep rule for one snapshot copy: the newest-N
// surviving copies, PLUS the newest complete archive while it is still inside the
// pin window. Incomplete (degraded) archives still fill newest-N so they remain
// the latest restore point for the instances they did capture, but they must not
// evict the last fully restorable copy.
//
// Both rules are always evaluated, never short-circuited: a complete archive that
// newest-N already protects must still consume the complete slot, or the next
// complete archive down would be pinned as well.
func snapshotRetentionProtects(floor *retentionFloor, completeKept *bool, present, isComplete, pinEligible bool) retentionVerdict {
	newest := floor.protects(present)
	pinned := false
	pinExpired := false
	if present && isComplete && !*completeKept {
		// This is the newest surviving complete copy. Consume the slot whether
		// or not the pin still holds it: every complete archive below it is
		// older still, so falling through to pin one of those would keep a
		// strictly staler copy for strictly longer.
		*completeKept = true
		pinned = pinEligible
		pinExpired = !pinEligible
	}
	switch {
	case newest:
		return retentionVerdict{keep: retentionKeepNewest}
	case pinned:
		return retentionVerdict{keep: retentionKeepNewestComplete}
	default:
		return retentionVerdict{pinExpired: pinExpired}
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
