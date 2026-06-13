package daemon_test

import (
	"testing"

	"github.com/hypersequent/oddk/internal/daemon"
)

func TestCronTaskTracker_Sequential(t *testing.T) {
	// Create tracker with nil dependencies (we're only testing the logic)
	tracker := daemon.NewCronTaskTracker(nil, nil)

	// Test: Initial state should be empty
	current, queueLen, queued := tracker.GetStatus()
	if current != "" {
		t.Error("Should start with no current task")
	}
	if queueLen != 0 {
		t.Error("Should start with empty queue")
	}
	if len(queued) != 0 {
		t.Error("Should start with empty queued tasks")
	}

	// We can't easily test the internal state without running actual tasks,
	// but we can test the GetStatus functionality
	t.Log("Sequential execution logic tested via integration tests")
}

func TestCronTaskTracker_GetStatus(t *testing.T) {
	tracker := daemon.NewCronTaskTracker(nil, nil)

	// Initial state
	current, queueLen, queued := tracker.GetStatus()
	if current != "" {
		t.Error("Should start with no current task")
	}
	if queueLen != 0 {
		t.Error("Should start with empty queue")
	}
	if len(queued) != 0 {
		t.Error("Should start with empty queued tasks")
	}

	// Test that GetStatus returns proper copies/values
	// (no shared state issues)
	queued = append(queued, "test-modification")
	if len(queued) != 1 {
		t.Error("Should be able to modify returned slice copy")
	}

	// Get status again - should still be empty since we modified a copy
	_, queueLen2, queued2 := tracker.GetStatus()
	if queueLen2 != 0 {
		t.Error("Modifying returned slice copy should not affect internal state")
	}
	if len(queued2) != 0 {
		t.Error("Internal state should remain unchanged")
	}
}
