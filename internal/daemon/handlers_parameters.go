package daemon

import (
	"encoding/json"
	"net/http"

	"github.com/hypersequent/oddk/internal/operations"
)

func (s *Server) handleParameterGroupGet(w http.ResponseWriter, r *http.Request) {
	groupName := r.PathValue("name")
	if groupName == "" {
		s.writeError(w, http.StatusBadRequest, "Parameter group name is required")
		return
	}

	params := operations.ParameterGroupGetParams{
		Name: groupName,
	}

	result, err := operations.ParameterGroupGet(r.Context(), s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleParameterGroupList(w http.ResponseWriter, r *http.Request) {
	params := operations.ParameterGroupListParams{}

	result, err := operations.ParameterGroupList(r.Context(), s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleParameterGroupPut(w http.ResponseWriter, r *http.Request) {
	groupName := r.PathValue("name")
	if groupName == "" {
		s.writeError(w, http.StatusBadRequest, "Parameter group name is required")
		return
	}

	var req struct {
		Parameters json.RawMessage `json:"parameters"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if len(req.Parameters) == 0 {
		s.writeError(w, http.StatusBadRequest, "Parameters are required")
		return
	}

	params := operations.ParameterGroupPutParams{
		Name:       groupName,
		Parameters: req.Parameters,
	}

	result, err := operations.ParameterGroupPut(r.Context(), s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.writeJSON(w, http.StatusCreated, result)
}

func (s *Server) handleParameterGroupDelete(w http.ResponseWriter, r *http.Request) {
	groupName := r.PathValue("name")
	if groupName == "" {
		s.writeError(w, http.StatusBadRequest, "Parameter group name is required")
		return
	}

	params := operations.ParameterGroupDeleteParams{
		Name: groupName,
	}

	result, err := operations.ParameterGroupDelete(r.Context(), s.opDeps, params)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}
