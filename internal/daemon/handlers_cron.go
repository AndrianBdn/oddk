package daemon

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/andrianbdn/oddk/internal/operations"
)

type CronPlanRequest struct {
	InstanceName      string `json:"instanceName"`
	UTCHour           *int   `json:"utcHour"`
	CleanupLocalDays  *int   `json:"cleanupLocalDays"`
	CleanupRemoteDays *int   `json:"cleanupRemoteDays"`
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

	existing, err := s.store.Cron.GetPlan(req.InstanceName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("read existing backup plan: %v", err))
		return
	}

	utcHour := 3
	cleanupLocal := 7
	cleanupRemote := 14
	if existing != nil {
		utcHour = existing.UTCHour
		cleanupLocal = existing.CleanupLocalDays
		cleanupRemote = existing.CleanupRemoteDays
	} else if req.UTCHour == nil {
		s.writeError(w, http.StatusBadRequest, "UTC hour is required when creating a new backup schedule")
		return
	}
	if req.UTCHour != nil {
		utcHour = *req.UTCHour
	}
	if req.CleanupLocalDays != nil {
		cleanupLocal = *req.CleanupLocalDays
	}
	if req.CleanupRemoteDays != nil {
		cleanupRemote = *req.CleanupRemoteDays
	}

	if utcHour < 0 || utcHour > 23 {
		s.writeError(w, http.StatusBadRequest, "UTC hour must be between 0 and 23")
		return
	}
	if cleanupLocal < 1 {
		s.writeError(w, http.StatusBadRequest, "cleanup-local-days must be at least 1")
		return
	}
	if cleanupRemote < 1 {
		s.writeError(w, http.StatusBadRequest, "cleanup-remote-days must be at least 1")
		return
	}

	op := operations.NewCronBackupCreateOp(s.opDeps, req.InstanceName, utcHour, cleanupLocal, cleanupRemote)

	if err := s.executor.Execute(r.Context(), op); err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to create cron backup: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, op.GetResult())
}

func (s *Server) handleCronBackupList(w http.ResponseWriter, r *http.Request) {
	op := operations.NewCronBackupListOp(s.opDeps)

	if err := s.executor.ExecuteRead(r.Context(), op); err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to list cron backups: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, op.GetResult())
}

// handleCronLogs serves GET /api/cron/logs?limit=N&instance=NAME.
//
// Deliberately does NOT go through the executor: this is a plain read of
// cron_logs, and the whole point of the endpoint is to answer "why did last
// night's snapshot fail" — which must stay answerable while a long operation
// holds the executor lock. (/api/snapshots/remote skips it for the same reason.)
func (s *Server) handleCronLogs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil {
			limit = parsed
		}
	}

	result, err := operations.CronLogs(s.opDeps, operations.CronLogsParams{
		Instance: r.URL.Query().Get("instance"),
		Limit:    limit,
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
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

// handleCronBackupPause and handleCronBackupResume suspend/restore ONE
// instance's backup schedule. Separate from the create/update endpoint for the
// same reason as the snapshot pair: editing a field must never resume a
// schedule that `snapshot apply` paused to keep this host out of another
// deployment's offsite bucket.
func (s *Server) handleCronBackupPause(w http.ResponseWriter, r *http.Request) {
	instance := r.PathValue("instance")
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "paused by an operator"
	}
	n, err := s.store.Cron.PausePlan(instance, reason)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("pause backup schedule: %v", err))
		return
	}
	if n == 0 {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("no backup schedule for instance %s", instance))
		return
	}
	s.respondWithBackupPlan(w, instance)
}

func (s *Server) handleCronBackupResume(w http.ResponseWriter, r *http.Request) {
	instance := r.PathValue("instance")
	n, err := s.store.Cron.ResumePlan(instance)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("resume backup schedule: %v", err))
		return
	}
	if n == 0 {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("no backup schedule for instance %s", instance))
		return
	}
	s.respondWithBackupPlan(w, instance)
}

func (s *Server) respondWithBackupPlan(w http.ResponseWriter, instance string) {
	plan, err := s.store.Cron.GetPlan(instance)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("read backup schedule: %v", err))
		return
	}
	s.writeJSON(w, http.StatusOK, plan)
}
