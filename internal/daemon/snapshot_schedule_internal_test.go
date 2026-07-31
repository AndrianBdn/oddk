package daemon

import (
	"testing"
	"time"
)

// TestSnapshotSlotStartDoesNotDrift pins the dedup boundary used by
// checkSnapshotPlan.
//
// The scheduler must ask "has a run started since the top of this scheduled
// hour", NOT "since now minus the interval". The sliding form drifts: a run
// recorded at 03:45 keeps a 24h window closed until 03:46 the next day, so each
// run lands a minute later than the last. After roughly fifteen days the hour
// rolls over to 04:00, RunsAtHour stops matching a plan anchored at 03, and
// snapshots silently never run again.
//
// This test simulates both forms over 40 days and requires the slot form to keep
// firing in the anchored hour throughout.
func TestSnapshotSlotStartDoesNotDrift(t *testing.T) {
	const anchorHour = 3

	// The PRODUCTION boundary function, not a copy of it — a re-implementation
	// here would keep passing after the real one regressed.
	slotStart := snapshotSlotStart

	// Model a run happening at the first minute the dedup boundary permits,
	// bounded below by minute 31 where the jitter ladder guarantees a run.
	//
	// The SECONDS matter and are the whole mechanism: the scheduler ticks on the
	// minute but started_at is written with sub-minute precision, so a run
	// recorded at 03:45:12 is still strictly after a cutoff of 03:45:00. Modelling
	// runs as landing exactly on :00 would hide the drift entirely.
	const runSecond = 12
	lastRun := time.Date(2026, 1, 1, anchorHour, 45, runSecond, 0, time.UTC)
	for day := 2; day <= 40; day++ {
		var fired bool
		for minute := range 60 {
			now := time.Date(2026, 1, day, anchorHour, minute, 0, 0, time.UTC)
			// Dedup: did a run already start in this slot?
			if !lastRun.After(slotStart(now)) && minute > 30 {
				lastRun = time.Date(2026, 1, day, anchorHour, minute, runSecond, 0, time.UTC)
				fired = true
				break
			}
		}
		if !fired {
			t.Fatalf("day %d: no snapshot fired in the anchored hour - the schedule has drifted out of its slot", day)
		}
		if lastRun.Hour() != anchorHour {
			t.Fatalf("day %d: run landed at %02d:%02d, outside the anchored hour %02d",
				day, lastRun.Hour(), lastRun.Minute(), anchorHour)
		}
	}

	// And prove the sliding window this replaced genuinely does drift, so the
	// test above is guarding something real rather than restating the obvious.
	slidingLast := time.Date(2026, 1, 1, anchorHour, 45, runSecond, 0, time.UTC)
	drifted := false
	for day := 2; day <= 40; day++ {
		fired := false
		for minute := range 60 {
			now := time.Date(2026, 1, day, anchorHour, minute, 0, 0, time.UTC)
			if !slidingLast.After(now.Add(-24*time.Hour)) && minute > 30 {
				slidingLast = time.Date(2026, 1, day, anchorHour, minute, runSecond, 0, time.UTC)
				fired = true
				break
			}
		}
		if !fired {
			drifted = true
			break
		}
	}
	if !drifted {
		t.Error("the sliding now-minus-interval window did NOT drift in this model; the model no longer demonstrates the bug being guarded against")
	}
}
