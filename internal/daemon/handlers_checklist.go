package daemon

import (
	"context"
	"fmt"
	"net/http"

	"github.com/andrianbdn/oddk/internal/operations"
)

// handleChecklist handles GET /api/checklist - audit snapshot of all
// instances (health, parameter group, backups) plus notification status
func (s *Server) handleChecklist(w http.ResponseWriter, r *http.Request) {
	// The per-instance consistency checks probe PostgreSQL with a 5s timeout
	// each, so a deployment with several broken instances can legitimately
	// exceed the server's WriteTimeout.
	s.clearWriteDeadline(w, "checklist")

	op := operations.NewChecklistOp(s.opDeps)

	if err := s.executor.ExecuteRead(context.Background(), op); err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to build checklist: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, op.GetResult())
}
