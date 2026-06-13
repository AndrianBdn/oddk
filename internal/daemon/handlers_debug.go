//go:build oddk_debug

// This file holds the debug-only backup time-shift HTTP handler. It is compiled
// in only under the `oddk_debug` build tag (used by the e2e suite) and never
// ships in production binaries; its route is registered by registerDebugRoutes
// in router_debug.go (the production build gets the no-op stub).
package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hypersequent/oddk/internal/operations"
	"github.com/hypersequent/oddk/internal/store/kvstore"
)

type BackupTimeShiftRequest struct {
	BackupID int `json:"backupId"`
	DaysBack int `json:"daysBack"`
}

func (s *Server) handleDebugBackupTimeShift(w http.ResponseWriter, r *http.Request) {
	// Check if debug time machine is enabled
	timeMachineEnabled, err := s.opDeps.Store.KV.GetInt(kvstore.KeyBackupDebugTimeMachine)
	if err != nil || timeMachineEnabled != 1 {
		s.writeError(w, http.StatusBadRequest, "Debug time machine is not enabled. Set backup.debug_time_machine.int to 1")
		return
	}

	var req BackupTimeShiftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.BackupID <= 0 {
		s.writeError(w, http.StatusBadRequest, "Invalid backup ID")
		return
	}

	if req.DaysBack <= 0 {
		s.writeError(w, http.StatusBadRequest, "Days back must be positive")
		return
	}

	if req.DaysBack > 365 {
		s.writeError(w, http.StatusBadRequest, "Days back cannot exceed 365")
		return
	}

	op := operations.NewBackupTimeShiftOp(s.opDeps, req.BackupID, req.DaysBack)

	if err := s.executor.Execute(r.Context(), op); err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to shift backup time: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, op.GetResult())
}
