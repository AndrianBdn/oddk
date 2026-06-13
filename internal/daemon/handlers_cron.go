package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hypersequent/oddk/internal/operations"
)

type CronPlanRequest struct {
	InstanceName      string `json:"instanceName"`
	UTCHour           int    `json:"utcHour"`
	CleanupLocalDays  int    `json:"cleanupLocalDays"`
	CleanupRemoteDays int    `json:"cleanupRemoteDays"`
}

func (s *Server) handleCronBackupCreate(w http.ResponseWriter, r *http.Request) {
	var req CronPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.InstanceName == "" {
		s.writeError(w, http.StatusBadRequest, "Instance name is required")
		return
	}

	if req.UTCHour < 0 || req.UTCHour > 23 {
		s.writeError(w, http.StatusBadRequest, "UTC hour must be between 0 and 23")
		return
	}

	// Set defaults if not provided
	if req.CleanupLocalDays == 0 {
		req.CleanupLocalDays = 7
	}
	if req.CleanupRemoteDays == 0 {
		req.CleanupRemoteDays = 14
	}

	if req.CleanupLocalDays < 1 {
		s.writeError(w, http.StatusBadRequest, "cleanup-local-days must be at least 1")
		return
	}
	if req.CleanupRemoteDays < 1 {
		s.writeError(w, http.StatusBadRequest, "cleanup-remote-days must be at least 1")
		return
	}

	op := operations.NewCronBackupCreateOp(s.opDeps, req.InstanceName, req.UTCHour, req.CleanupLocalDays, req.CleanupRemoteDays)

	if err := s.executor.Execute(r.Context(), op); err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to create cron backup: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, op.GetResult())
}

func (s *Server) handleCronBackupList(w http.ResponseWriter, r *http.Request) {
	op := operations.NewCronBackupListOp(s.opDeps)

	if err := s.executor.Execute(r.Context(), op); err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to list cron backups: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, op.GetResult())
}

func (s *Server) handleCronBackupDelete(w http.ResponseWriter, r *http.Request) {
	instanceName := r.PathValue("instance")
	if instanceName == "" {
		s.writeError(w, http.StatusBadRequest, "Instance name is required")
		return
	}

	op := operations.NewCronBackupDeleteOp(s.opDeps, instanceName)

	if err := s.executor.Execute(r.Context(), op); err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to delete cron backup: %v", err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
