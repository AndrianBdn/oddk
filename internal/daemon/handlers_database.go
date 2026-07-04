package daemon

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/hypersequent/oddk/internal/operations"
)

// handleListDatabases handles GET /api/rdbms/{name}/databases
func (s *Server) handleListDatabases(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	params := operations.ListDatabasesParams{
		InstanceName: name,
	}

	result, err := s.executor.ListDatabasesOp(context.Background(), s.opDeps, params)
	if err != nil {
		s.writeOpError(w, err)
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

// handleCreateDatabase handles POST /api/rdbms/{name}/databases
func (s *Server) handleCreateDatabase(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	var req struct {
		DatabaseName string `json:"databaseName"`
		Username     string `json:"username"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.DatabaseName == "" {
		s.writeError(w, http.StatusBadRequest, "database name is required")
		return
	}

	params := operations.CreateDatabaseParams{
		InstanceName: name,
		DatabaseName: req.DatabaseName,
		Username:     req.Username,
	}

	result, err := s.executor.CreateDatabaseOp(context.Background(), s.opDeps, params)
	if err != nil {
		s.writeOpError(w, err)
		return
	}

	s.writeJSON(w, http.StatusCreated, result)
}

// handleAddDatabaseUser handles POST /api/rdbms/{name}/databases/{database}/users
func (s *Server) handleAddDatabaseUser(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	database, ok := s.extractPathParam(w, r, "database")
	if !ok {
		return
	}

	var req struct {
		Username string `json:"username"`
		ReadOnly bool   `json:"readOnly"`
		Owner    bool   `json:"owner"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Username == "" {
		s.writeError(w, http.StatusBadRequest, "username is required")
		return
	}

	params := operations.AddDatabaseUserParams{
		InstanceName: name,
		DatabaseName: database,
		Username:     req.Username,
		ReadOnly:     req.ReadOnly,
		Owner:        req.Owner,
	}

	result, err := s.executor.AddDatabaseUserOp(context.Background(), s.opDeps, params)
	if err != nil {
		s.writeOpError(w, err)
		return
	}

	s.writeJSON(w, http.StatusCreated, result)
}

// handleDeleteDatabaseUser handles DELETE /api/rdbms/{name}/users/{username}
func (s *Server) handleDeleteDatabaseUser(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	username, ok := s.extractPathParam(w, r, "username")
	if !ok {
		return
	}

	params := operations.DeleteDatabaseUserParams{
		InstanceName: name,
		Username:     username,
	}

	result, err := s.executor.DeleteDatabaseUserOp(context.Background(), s.opDeps, params)
	if err != nil {
		s.writeOpError(w, err)
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

// handleResetDatabaseUserPassword handles PUT /api/rdbms/{name}/users/{username}/password
func (s *Server) handleResetDatabaseUserPassword(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	username, ok := s.extractPathParam(w, r, "username")
	if !ok {
		return
	}

	params := operations.ResetDatabaseUserPasswordParams{
		InstanceName: name,
		Username:     username,
	}

	result, err := s.executor.ResetDatabaseUserPasswordOp(context.Background(), s.opDeps, params)
	if err != nil {
		s.writeOpError(w, err)
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}
