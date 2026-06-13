package daemon

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/hypersequent/oddk/internal/operations"
)

// Request types for image operations
type PullImageRequest struct {
	Version string `json:"version"`
	Image   string `json:"image"`
}

// handlePullImage handles POST /api/pull
func (s *Server) handlePullImage(w http.ResponseWriter, r *http.Request) {
	var req PullImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Version == "" && req.Image == "" {
		s.writeError(w, http.StatusBadRequest, "version or image is required")
		return
	}

	params := operations.PullImageParams{
		Version: req.Version,
		Image:   req.Image,
	}

	op := operations.NewPullImageOp(s.opDeps, params)

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	result := op.GetResult()
	s.writeJSON(w, http.StatusOK, result)
}
