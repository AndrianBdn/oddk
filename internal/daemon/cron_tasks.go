package daemon

import (
	"context"
	"log"
	"slices"
	"sync"

	"github.com/hypersequent/oddk/internal/operations"
)

// CronTaskTracker manages sequential execution of cron tasks
type CronTaskTracker struct {
	mu           sync.Mutex
	currentTask  string   // Currently running task instance name (empty if none)
	pendingTasks []string // Queue of tasks waiting to run
	opDeps       *operations.Dependencies
	executor     *operations.Executor
}

// NewCronTaskTracker creates a new cron task tracker
func NewCronTaskTracker(opDeps *operations.Dependencies, executor *operations.Executor) *CronTaskTracker {
	return &CronTaskTracker{
		currentTask:  "",
		pendingTasks: make([]string, 0),
		opDeps:       opDeps,
		executor:     executor,
	}
}

// RunTask runs a cron task sequentially - starts immediately if nothing running, queues if busy
// Returns true if the task was started or queued, false if it's a duplicate
func (t *CronTaskTracker) RunTask(ctx context.Context, instanceName string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Check if this instance is already running
	if t.currentTask == instanceName {
		log.Printf("Cron task for instance %s is already running, skipping", instanceName)
		return false
	}

	// Check if this instance is already queued
	if slices.Contains(t.pendingTasks, instanceName) {
		log.Printf("Cron task for instance %s is already queued, skipping", instanceName)
		return false
	}

	// If nothing is running, start this task immediately
	if t.currentTask == "" {
		t.currentTask = instanceName
		log.Printf("Starting cron task for instance %s", instanceName)
		go t.executeTask(ctx, instanceName)
		return true
	}

	// Something else is running, queue this task
	t.pendingTasks = append(t.pendingTasks, instanceName)
	log.Printf("Cron task for instance %s queued (queue length: %d)", instanceName, len(t.pendingTasks))
	return true
}

// executeTask runs a single cron task and then checks for next queued task
func (t *CronTaskTracker) executeTask(ctx context.Context, instanceName string) {
	cronTaskOp := operations.NewCronTaskOp(t.opDeps, instanceName)
	if err := t.executor.Execute(ctx, cronTaskOp); err != nil {
		log.Printf("Cron task failed for instance %s: %v", instanceName, err)
	} else {
		log.Printf("Cron task completed successfully for instance %s", instanceName)
	}

	// Task finished, check if there's a next task to run
	t.mu.Lock()
	t.currentTask = "" // Mark current task as finished

	if len(t.pendingTasks) > 0 {
		nextTask := t.pendingTasks[0]
		t.pendingTasks = t.pendingTasks[1:] // Remove from queue
		t.currentTask = nextTask            // Mark as running

		log.Printf("Starting next queued cron task for instance %s (queue length: %d)", nextTask, len(t.pendingTasks))
		t.mu.Unlock()

		// Start the next task (recursive call in same goroutine to avoid spawning too many)
		t.executeTask(ctx, nextTask)
	} else {
		log.Printf("No more cron tasks in queue")
		t.mu.Unlock()
	}
}

// GetStatus returns the current status for debugging
func (t *CronTaskTracker) GetStatus() (currentTask string, queueLength int, queuedTasks []string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	queuedTasks = make([]string, len(t.pendingTasks))
	copy(queuedTasks, t.pendingTasks)

	return t.currentTask, len(t.pendingTasks), queuedTasks
}
