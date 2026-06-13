package util

import (
	"sync"
	"time"
)

// Counter provides monotonic counters for unique naming within the same timestamp
type Counter struct {
	mu       sync.Mutex
	counters map[string]int // map of timestamp -> counter
}

// NewCounter creates a new monotonic counter
func NewCounter() *Counter {
	return &Counter{
		counters: make(map[string]int),
	}
}

// GetNext returns the next counter value for a given timestamp
func (c *Counter) GetNext(timestamp string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Clean up old entries (older than 5 minutes) to prevent memory leak
	// Only clean up if we have many entries to avoid performance impact
	if len(c.counters) > 100 {
		now := time.Now().UTC()
		for ts := range c.counters {
			if len(ts) >= 14 {
				tsTime, err := time.Parse("20060102150405", ts)
				if err == nil {
					age := now.Sub(tsTime)
					if age > 5*time.Minute {
						delete(c.counters, ts)
					}
				}
			}
		}
	}

	c.counters[timestamp]++
	return c.counters[timestamp]
}

// Len returns the number of counters (for testing)
func (c *Counter) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.counters)
}

// BackupCounter is a global counter for backup naming
var BackupCounter = NewCounter()
