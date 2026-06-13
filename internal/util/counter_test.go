package util_test

import (
	"testing"
	"time"

	"github.com/hypersequent/oddk/internal/util"
)

func TestCounter(t *testing.T) {
	c := util.NewCounter()

	// Use current UTC time for realistic test
	now := time.Now().UTC()
	ts := now.Format("20060102150405")

	// First call should return 1
	if got := c.GetNext(ts); got != 1 {
		t.Errorf("First counter should be 1, got %d", got)
	}

	// Second call should return 2
	if got := c.GetNext(ts); got != 2 {
		t.Errorf("Second counter should be 2, got %d", got)
	}

	// Third call should return 3
	if got := c.GetNext(ts); got != 3 {
		t.Errorf("Third counter should be 3, got %d", got)
	}

	// Different timestamp should start at 1
	ts2 := now.Add(time.Second).Format("20060102150405")
	if got := c.GetNext(ts2); got != 1 {
		t.Errorf("Counter for new timestamp should be 1, got %d", got)
	}

	// Original timestamp should continue at 4
	if got := c.GetNext(ts); got != 4 {
		t.Errorf("Fourth counter should be 4, got %d", got)
	}
}

func TestCounterCleanup(t *testing.T) {
	c := util.NewCounter()

	// Create 101 unique old timestamps (spread over 101 seconds, 10 minutes ago)
	baseTime := time.Now().UTC().Add(-10 * time.Minute)
	oldTimestamps := make([]string, 101)
	for i := range 101 {
		oldTimestamps[i] = baseTime.Add(time.Duration(i) * time.Second).Format("20060102150405")
		c.GetNext(oldTimestamps[i])
	}

	// Verify we have 101 entries
	if c.Len() != 101 {
		t.Errorf("Expected 101 entries, got %d", c.Len())
	}

	// Add a current entry - this should trigger cleanup
	currentTs := time.Now().UTC().Format("20060102150405")
	c.GetNext(currentTs)

	// Old entries should be cleaned up, only current entry should remain
	if c.Len() != 1 {
		t.Errorf("Expected 1 entry after cleanup, got %d", c.Len())
	}

	// Verify we can still get counters (implicitly tests that current entry exists)
	if got := c.GetNext(currentTs); got != 2 {
		t.Errorf("Expected counter 2 for current timestamp, got %d", got)
	}
}
