package daemon

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/hypersequent/oddk/internal/services"
)

// HealthScheduler manages the health check scheduling with process mutex
type HealthScheduler struct {
	healthChecker *services.HealthChecker
	intervalSec   int // 0 means disabled
	mutex         sync.Mutex
	running       bool
	paused        bool
}

func NewHealthScheduler(healthChecker *services.HealthChecker, intervalSec int) *HealthScheduler {
	return &HealthScheduler{
		healthChecker: healthChecker,
		intervalSec:   intervalSec,
	}
}

func (hs *HealthScheduler) Start(ctx context.Context) {
	if hs.intervalSec <= 0 {
		log.Println("Health check scheduler disabled (interval <= 0)")
		return
	}

	log.Printf("Starting health check scheduler (%ds cadence)", hs.intervalSec)

	// Reset any stuck in_progress records on startup
	if err := hs.healthChecker.ResetInProgressRecords(); err != nil {
		log.Printf("Error resetting in_progress health records: %v", err)
	}

	ticker := time.NewTicker(time.Duration(hs.intervalSec) * time.Second)
	defer ticker.Stop()

	// Run initial health check immediately, then follow ticker schedule
	hs.runHealthCheck(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("Health check scheduler shutting down")
			return
		case <-ticker.C:
			hs.runHealthCheck(ctx)
		}
	}
}

// runHealthCheck executes a health check if not already running (process mutex)
func (hs *HealthScheduler) runHealthCheck(ctx context.Context) {
	// Try to acquire lock (process mutex)
	if !hs.mutex.TryLock() {
		log.Println("Health check already running, skipping")
		return
	}
	defer hs.mutex.Unlock()

	// Check if paused
	if hs.paused {
		log.Println("Health check paused, skipping")
		return
	}

	hs.running = true
	defer func() {
		hs.running = false
	}()

	log.Println("Starting health check run")
	start := time.Now()

	checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	if err := hs.healthChecker.RunHealthCheck(checkCtx); err != nil {
		log.Printf("Health check failed: %v", err)
	} else {
		duration := time.Since(start)
		log.Printf("Health check completed in %v", duration)
	}
}

// Pause pauses health checks (new ones won't start)
func (hs *HealthScheduler) Pause() {
	hs.mutex.Lock()
	defer hs.mutex.Unlock()
	hs.paused = true
}

// Unpause resumes health checks
func (hs *HealthScheduler) Unpause() {
	hs.mutex.Lock()
	defer hs.mutex.Unlock()
	hs.paused = false
}

// WaitForCompletion waits for any running health check to complete
func (hs *HealthScheduler) WaitForCompletion(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		hs.mutex.Lock()
		running := hs.running
		hs.mutex.Unlock()

		if !running {
			return true // No health check running
		}

		time.Sleep(100 * time.Millisecond)
	}

	return false // Timeout reached
}

// IsRunning returns whether a health check is currently running
func (hs *HealthScheduler) IsRunning() bool {
	return hs.running
}
