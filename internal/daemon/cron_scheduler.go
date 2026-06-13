package daemon

import (
	"context"
	"log"
	"math/rand"
	"time"

	"github.com/hypersequent/oddk/internal/store/cron"
	"github.com/hypersequent/oddk/internal/store/kvstore"
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
		// die each minute with escalating probability (5% at min 1-9, 10% at
		// 10-20, then 100% after min 30 so the task is guaranteed to run before
		// the hour ends). This spreads backup load across the hour. The
		// HasRunInLastHour check above is the once-per-hour dedup guard that
		// makes the probabilistic retry safe: a plan that wins an early roll is
		// not triggered again later in the same hour. (forceRun / the debug
		// ticker collapse this to deterministic runs for tests.)
		var probability float64
		if forceRun {
			probability = 1.0 // Always run in force mode
		} else {
			// Normal probability based on current minute
			switch {
			case currentMinute >= 1 && currentMinute <= 9:
				probability = 0.05 // 5% chance
			case currentMinute >= 10 && currentMinute <= 20:
				probability = 0.10 // 10% chance
			case currentMinute > 30:
				probability = 1.0 // 100% chance
			default:
				continue // Don't run in minute 0 or 21-30
			}
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
