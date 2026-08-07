package daemon

import (
	"context"
	"log"
	"math/rand"
	"time"

	"github.com/andrianbdn/oddk/internal/operations"
	"github.com/andrianbdn/oddk/internal/store/cron"
	"github.com/andrianbdn/oddk/internal/store/kvstore"
)

// Use a local random generator to avoid global state
// #nosec G404 - Using math/rand for cron scheduling jitter, not cryptography
var rng = rand.New(rand.NewSource(time.Now().UnixNano()))

// startCronScheduler runs the cron scheduler that checks every minute for tasks to run
func (s *Server) startCronScheduler(ctx context.Context) {
	log.Println("Starting cron scheduler")

	interval := 60 * time.Second // default 60 seconds
	if debugInterval, err := s.store.KV.GetInt(kvstore.KeyCronDebugTickerInterval); err == nil && debugInterval > 0 {
		interval = time.Duration(debugInterval) * time.Second
		log.Printf("Using debug cron ticker interval: %v", interval)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Cron scheduler shutting down")
			return
		case <-ticker.C:
			s.checkAndRunCronTasks(ctx)
		}
	}
}

// checkSnapshotPlan decides whether the deployment-wide snapshot should run now.
//
// It mirrors the per-instance path — dedup first, then the same jitter ladder so
// a snapshot does not stampede at :00 — but the dedup window is the plan's
// INTERVAL rather than a fixed hour, because a snapshot plan may fire several
// times a day. HasRunSince counts cron_logs rows, which are written when a run
// starts, so a snapshot that fails is not retried every minute for the rest of
// its window.
func (s *Server) checkSnapshotPlan(ctx context.Context, now time.Time, currentMinute int, forceRun bool) {
	plan, err := s.store.Snapshot.GetPlan()
	if err != nil {
		log.Printf("Error getting snapshot plan: %v", err)
		return
	}
	if plan == nil {
		return // no snapshot schedule configured
	}
	if !forceRun && !plan.RunsAtHour(now.Hour()) {
		return
	}

	// Dedup against the START OF THIS SLOT, not a sliding now-minus-interval
	// window. The sliding form drifts and eventually stops the schedule dead:
	// a run recorded at 03:45 keeps the window closed until 03:46 the next day,
	// so each run lands a minute later than the last, and after ~15 days the
	// hour rolls over to 04:00 — at which point RunsAtHour no longer matches a
	// plan anchored at 03 and snapshots silently never run again.
	//
	// The slot boundary is exact: RunsAtHour has already established that this
	// hour is a scheduled one, so "has a run started since the top of this hour"
	// is precisely the question, and it cannot drift.
	hasRun, err := s.store.Cron.HasRunSince(operations.SnapshotCronInstance, snapshotSlotStart(now))
	if err != nil {
		log.Printf("Error checking recent snapshot runs: %v", err)
		return
	}
	if hasRun {
		return
	}

	probability := snapshotRunProbability(currentMinute, forceRun)
	if probability == 0 {
		return
	}
	if rng.Float64() >= probability {
		log.Printf("Skipping scheduled snapshot this minute (probability %.0f%%)", probability*100)
		return
	}

	log.Printf("Attempting scheduled snapshot (hour %d, minute %d, every %dh anchored at %02d:00 UTC)",
		now.Hour(), currentMinute, plan.IntervalHours, plan.UTCHour)
	s.cronTracker.RunTask(ctx, operations.SnapshotCronInstance)
}

// snapshotSlotStart returns the top of the scheduled hour containing now.
//
// This is the snapshot dedup boundary, and it must NOT be a sliding
// now-minus-interval window. The scheduler ticks on the minute while started_at
// is written with sub-minute precision, so a run recorded at 03:45:12 stays
// strictly after a sliding cutoff of 03:45:00 — pushing the next run to 03:46,
// then 03:47, a minute later every period. After roughly fifteen days a daily
// plan's hour rolls over, RunsAtHour stops matching the anchor, and snapshots
// silently never run again. Anchoring to the slot cannot drift.
func snapshotSlotStart(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), utc.Hour(), 0, 0, 0, time.UTC)
}

// snapshotRunProbability is the per-instance jitter ladder, extracted so both
// paths spread load the same way. 0 means "do not run this minute".
func snapshotRunProbability(currentMinute int, forceRun bool) float64 {
	if forceRun {
		return 1.0
	}
	switch {
	case currentMinute >= 1 && currentMinute <= 9:
		return 0.05
	case currentMinute >= 10 && currentMinute <= 20:
		return 0.10
	case currentMinute > 30:
		return 1.0
	default:
		return 0
	}
}

// maybeSweepSnapshotDownloads prunes the managed downloads area once a day.
// Startup covers restarted daemons; this covers ones that run for months. It
// runs on the scheduler goroutine only, so the timestamp needs no lock — and
// the sweep itself is safe against the executor because in-flight downloads
// keep their temp files' mtimes fresh (SweepSnapshotDownloads only reaps aged
// entries).
func (s *Server) maybeSweepSnapshotDownloads() {
	if time.Since(s.lastDownloadsSweep) < 24*time.Hour {
		return
	}
	s.lastDownloadsSweep = time.Now()
	if pruned, err := operations.SweepSnapshotDownloads(s.opDeps.BackupDir); err != nil {
		log.Printf("Warning: snapshot downloads sweep skipped: %v", err)
	} else if pruned > 0 {
		log.Printf("Pruned %d aged archive(s) from the snapshot downloads area (re-fetchable from S3)", pruned)
	}
}

// checkAndRunCronTasks checks if any cron tasks should be run at the current time
func (s *Server) checkAndRunCronTasks(ctx context.Context) {
	now := time.Now().UTC()
	currentHour := now.Hour()
	currentMinute := now.Minute()

	forceRun := false
	if debugForce, err := s.store.KV.GetInt(kvstore.KeyCronDebugForceRun); err == nil && debugForce == 1 {
		forceRun = true
		log.Println("Cron debug force run mode is enabled")
	}

	// Get cron plans - all plans if force run, otherwise just for current hour
	var plans []*cron.CronPlan
	var err error
	if forceRun {
		plans, err = s.store.Cron.GetAllPlans()
		if err != nil {
			log.Printf("Error getting all cron plans: %v", err)
			return
		}
		log.Printf("Found %d total cron plan(s) (force run mode)", len(plans))
	} else {
		// Get plans for current hour only
		plans, err = s.store.Cron.GetPlansForHour(currentHour)
		if err != nil {
			log.Printf("Error getting cron plans for hour %d: %v", currentHour, err)
			return
		}
		if len(plans) > 0 {
			log.Printf("Found %d cron plan(s) for hour %d", len(plans), currentHour)
		}
	}

	// The deployment-wide snapshot plan is evaluated independently of the
	// per-instance ones: it lives in its own singleton table, fires on an
	// interval rather than a single hour, and must still be considered on a tick
	// where no instance plan matched.
	s.checkSnapshotPlan(ctx, now, currentMinute, forceRun)

	// Prune the snapshot downloads area daily. This lives on the scheduler
	// tick — not the snapshot cron task — because a host that only ever
	// restores foreign archives may have no snapshot plan at all, and its
	// downloads would otherwise only age out across daemon restarts.
	s.maybeSweepSnapshotDownloads()

	if len(plans) == 0 {
		return // No tasks scheduled
	}

	for _, plan := range plans {
		// Check if this task has already run in the last hour to avoid duplicates
		hasRun, err := s.store.Cron.HasRunInLastHour(plan.InstanceName)
		if err != nil {
			log.Printf("Error checking if cron task already ran for %s: %v", plan.InstanceName, err)
			continue
		}

		if hasRun {
			log.Printf("Cron task for instance %s already ran in the last hour, skipping", plan.InstanceName)
			continue
		}

		// Jittered start within the scheduled hour: a plan is pinned to a UTC
		// hour, but rather than firing every instance's backup at :00 we roll a
		// die each minute with the escalating snapshotRunProbability ladder
		// (guaranteed to run before the hour ends). This spreads backup load
		// across the hour. The HasRunInLastHour check above is the
		// once-per-hour dedup guard that makes the probabilistic retry safe: a
		// plan that wins an early roll is not triggered again later in the same
		// hour. (forceRun / the debug ticker collapse this to deterministic
		// runs for tests.)
		probability := snapshotRunProbability(currentMinute, forceRun)
		if probability == 0 {
			continue // Don't run in minute 0 or 21-30
		}

		// Roll the dice
		if rng.Float64() < probability {
			log.Printf("Attempting to run cron task for instance %s (minute %d, probability %.0f%%)",
				plan.InstanceName, currentMinute, probability*100)
			s.cronTracker.RunTask(ctx, plan.InstanceName)
		} else {
			log.Printf("Skipping cron task for instance %s this minute (probability %.0f%%)",
				plan.InstanceName, probability*100)
		}
	}
}
