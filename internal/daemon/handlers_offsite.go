package daemon

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/hypersequent/oddk/internal/operations"
)

func (s *Server) handleOffsiteInfo(w http.ResponseWriter, r *http.Request) {
	result, err := operations.OffsiteInfo(s.opDeps)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleOffsiteGet(w http.ResponseWriter, r *http.Request) {
	result, err := operations.OffsiteGet(s.opDeps)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleOffsiteApply(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("read request body: %v", err))
		return
	}

	// The body is already JSON from the client, just pass it through
	params := &operations.OffsiteApplyParams{
		ConfigJSON: body,
	}
	err = operations.OffsiteApply(s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

func (s *Server) handleOffsiteRemove(w http.ResponseWriter, r *http.Request) {
	err := operations.OffsiteRemove(s.opDeps)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

func (s *Server) handleOffsiteLogs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil {
			limit = parsedLimit
		}
	}

	params := &operations.OffsiteLogsParams{
		Limit: limit,
	}
	result, err := operations.OffsiteLogs(s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleOffsiteTest(w http.ResponseWriter, r *http.Request) {
	result, err := operations.OffsiteTest(s.opDeps)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}
