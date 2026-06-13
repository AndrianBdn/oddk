package daemon

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hypersequent/oddk/internal/store/health"
)

// HealthStatus represents the detailed health status response
type HealthStatus struct {
	Overall      string        `json:"overall"` // "healthy", "degraded", "unhealthy"
	LastCheck    *HealthRecord `json:"lastCheck"`
	CheckRunning bool          `json:"checkRunning"`
	Timestamp    string        `json:"timestamp"`
}

// HealthRecord represents a single health check record for API response
type HealthRecord struct {
	ID               int64    `json:"id"`
	Timestamp        string   `json:"timestamp"`
	TimestampUnix    int64    `json:"timestampUnix"`
	InProgress       bool     `json:"inProgress"`
	HealthyAll       bool     `json:"healthyAll"`
	HealthyHost      bool     `json:"healthyHost"`
	HealthyInstances []string `json:"healthyInstances"`
	BrokenInstances  []string `json:"brokenInstances"`
	FailDetails      string   `json:"failDetails"`
}

// HealthHistory represents the health history response
type HealthHistory struct {
	Records   []*HealthRecord `json:"records"`
	Timestamp string          `json:"timestamp"`
}

// handleHealthStatus handles GET /api/health/status - detailed health status
func (s *Server) handleHealthStatus(w http.ResponseWriter, r *http.Request) {
	record, err := s.store.Health.GetLatestHealthRecord()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get health status: %v", err))
		return
	}

	status := HealthStatus{
		CheckRunning: s.healthScheduler.IsRunning(),
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	}

	if record == nil {
		// No health checks run yet
		status.Overall = "unknown"
	} else {
		// Convert health record for API response
		status.LastCheck = convertHealthRecord(record)

		// Determine overall status
		switch {
		case record.InProgress:
			status.Overall = "checking"
		case record.HealthyAll:
			status.Overall = "healthy"
		case record.HealthyHost:
			status.Overall = "degraded" // Host OK but some instances broken
		default:
			status.Overall = "unhealthy"
		}
	}

	s.writeJSON(w, http.StatusOK, status)
}

// handleHealthHistory handles GET /api/health/history - health check history
func (s *Server) handleHealthHistory(w http.ResponseWriter, r *http.Request) {
	// Parse limit parameter (default: 50, max: 200)
	limit := 50
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil {
			limit = min(max(parsedLimit, 1), 200)
		}
	}

	records, err := s.store.Health.GetHealthHistory(limit)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get health history: %v", err))
		return
	}

	// Convert records for API response
	apiRecords := make([]*HealthRecord, len(records))
	for i, record := range records {
		apiRecords[i] = convertHealthRecord(record)
	}

	history := HealthHistory{
		Records:   apiRecords,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	s.writeJSON(w, http.StatusOK, history)
}

// convertHealthRecord converts a store health record to an API health record
func convertHealthRecord(record *health.HealthRecord) *HealthRecord {
	return &HealthRecord{
		ID:               record.ID,
		Timestamp:        record.GetTimestamp().UTC().Format(time.RFC3339),
		TimestampUnix:    record.TsUnix,
		InProgress:       record.InProgress,
		HealthyAll:       record.HealthyAll,
		HealthyHost:      record.HealthyHost,
		HealthyInstances: record.GetHealthyInstancesList(),
		BrokenInstances:  record.GetBrokenInstancesList(),
		FailDetails:      record.FailDetails,
	}
}
