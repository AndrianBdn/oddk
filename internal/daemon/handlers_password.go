package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hypersequent/oddk/internal/operations"
)

// handleGetPassword handles GET /api/rdbms/{name}/password
func (s *Server) handleGetPassword(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	op := operations.NewGetPasswordOp(s.opDeps, name)

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get password: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, op.GetResult())
}

// handleSetPassword handles PUT /api/rdbms/{name}/password
func (s *Server) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	var req struct {
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	if req.Password == "" {
		s.writeError(w, http.StatusBadRequest, "password cannot be empty")
		return
	}

	op := operations.NewSetPasswordOp(s.opDeps, name, req.Password)

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to set password: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, op.GetResult())
}
