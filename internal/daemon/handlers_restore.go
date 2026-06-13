package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hypersequent/oddk/internal/operations"
)

// RestoreRequest represents the request body for restore
type RestoreRequest struct {
	BackupID     int    `json:"backupId,omitempty"`
	FilePath     string `json:"filePath,omitempty"`
	DatabaseName string `json:"databaseName"`
	RestoreAs    string `json:"restoreAs,omitempty"`
}

// RestoreResponse represents the response from restore
type RestoreResponse struct {
	TargetDatabase string `json:"targetDatabase"`
	SourceBackup   string `json:"sourceBackup"`
	Message        string `json:"message"`
}

// handleRDBMSRestore handles POST /api/rdbms/{name}/restore
func (s *Server) handleRDBMSRestore(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	var req RestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.BackupID == 0 && req.FilePath == "" {
		s.writeError(w, http.StatusBadRequest, "either backupId or filePath must be provided")
		return
	}
	if req.BackupID != 0 && req.FilePath != "" {
		s.writeError(w, http.StatusBadRequest, "backupId and filePath are mutually exclusive")
		return
	}
	if req.DatabaseName == "" {
		s.writeError(w, http.StatusBadRequest, "databaseName is required")
		return
	}

	params := &operations.RestoreRDBMSParams{
		BackupID:     req.BackupID,
		FilePath:     req.FilePath,
		InstanceName: name,
		DatabaseName: req.DatabaseName,
		RestoreAs:    req.RestoreAs,
		BackupDir:    s.backupDir,
	}

	var restoreResult *operations.RestoreRDBMSResult
	op := &restoreOp{
		params: params,
		deps:   s.opDeps,
		result: &restoreResult,
	}

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	response := RestoreResponse{
		TargetDatabase: restoreResult.TargetDatabase,
		SourceBackup:   restoreResult.SourceBackup,
		Message:        restoreResult.Message,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// restoreOp implements the Operation interface for restore operations
type restoreOp struct {
	params *operations.RestoreRDBMSParams
	deps   *operations.Dependencies
	result **operations.RestoreRDBMSResult
}

func (op *restoreOp) Name() string {
	return fmt.Sprintf("RestoreRDBMS[%s->%s]", op.params.DatabaseName, op.params.InstanceName)
}

func (op *restoreOp) Type() operations.OpType {
	return operations.OpTypeWrite
}

func (op *restoreOp) Execute(ctx context.Context) error {
	result, err := operations.RestoreRDBMS(ctx, op.deps, op.params)
	if err != nil {
		return err
	}
	*op.result = result
	return nil
}
