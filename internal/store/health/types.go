package health

import (
	"strings"
	"time"
)

type HealthRecord struct {
	ID               int64  `db:"id"`
	TsUnix           int64  `db:"ts_unix"`
	InProgress       bool   `db:"in_progress"`
	HealthyAll       bool   `db:"healthy_all"`
	HealthyHost      bool   `db:"healthy_host"`
	HealthyInstances string `db:"healthy_instances"` // comma-separated
	BrokenInstances  string `db:"broken_instances"`  // comma-separated
	FailDetails      string `db:"fail_details"`
}

// GetTimestamp returns the timestamp as time.Time
func (h *HealthRecord) GetTimestamp() time.Time {
	return time.Unix(h.TsUnix, 0)
}

// GetHealthyInstancesList returns healthy instances as a slice
func (h *HealthRecord) GetHealthyInstancesList() []string {
	if h.HealthyInstances == "" {
		return []string{}
	}
	return splitCommaSeparated(h.HealthyInstances)
}

// GetBrokenInstancesList returns broken instances as a slice
func (h *HealthRecord) GetBrokenInstancesList() []string {
	if h.BrokenInstances == "" {
		return []string{}
	}
	return splitCommaSeparated(h.BrokenInstances)
}

func splitCommaSeparated(s string) []string {
	if s == "" {
		return []string{}
	}
	result := []string{}
	for part := range strings.SplitSeq(s, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
