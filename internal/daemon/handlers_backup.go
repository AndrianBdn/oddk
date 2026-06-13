package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/hypersequent/oddk/internal/operations"
)

type BackupRequest struct {
	Comment string `json:"comment,omitempty"`
}

type BackupResponse struct {
	BackupID   int       `json:"backupId"`
	BackupPath string    `json:"backupPath"`
	Size       int64     `json:"size"`
	Timestamp  time.Time `json:"timestamp"`
	Message    string    `json:"message"`
}

// handleRDBMSBackup handles POST /api/rdbms/{name}/backup
func (s *Server) handleRDBMSBackup(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	var req BackupRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	params := &operations.BackupRDBMSParams{
		Name:      name,
		BackupDir: s.backupDir,
		Comment:   req.Comment,
	}

	var backupResult *operations.BackupRDBMSResult
	op := &backupOp{
		params: params,
		deps:   s.opDeps,
		result: &backupResult,
	}

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	response := BackupResponse{
		BackupID:   backupResult.BackupID,
		BackupPath: backupResult.BackupPath,
		Size:       backupResult.Size,
		Timestamp:  backupResult.Timestamp,
		Message:    "Backup completed successfully",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// handleListBackups handles GET /api/rdbms/{name}/backups
func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	// Use the operation with consistency checks
	params := operations.ListBackupsParams{
		InstanceName: name,
	}

	var result *operations.ListBackupsResult
	op := &listBackupsOp{
		params: params,
		deps:   s.opDeps,
		result: &result,
	}

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result.Backups)
}

// backupOp implements the Operation interface for backup operations
type backupOp struct {
	params *operations.BackupRDBMSParams
	deps   *operations.Dependencies
	result **operations.BackupRDBMSResult
}

func (op *backupOp) Name() string {
	return "BackupRDBMS"
}

func (op *backupOp) Type() operations.OpType {
	return operations.OpTypeWrite
}

func (op *backupOp) Execute(ctx context.Context) error {
	result, err := operations.BackupRDBMS(ctx, op.deps, op.params)
	if err != nil {
		return err
	}
	*op.result = result
	return nil
}

// handleListAllBackups handles GET /api/backups
func (s *Server) handleListAllBackups(w http.ResponseWriter, r *http.Request) {
	// Use the operation to list all backups
	params := operations.ListBackupsParams{
		InstanceName: "", // Empty string means all instances
	}

	var result *operations.ListBackupsResult
	op := &listBackupsOp{
		params: params,
		deps:   s.opDeps,
		result: &result,
	}

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result.Backups)
}

// handleUploadBackup handles POST /api/rdbms/{name}/backup/{id}/upload
func (s *Server) handleUploadBackup(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	backupIDStr, ok := s.extractPathParam(w, r, "id")
	if !ok {
		return
	}

	backupID := 0
	if _, err := fmt.Sscanf(backupIDStr, "%d", &backupID); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid backup ID")
		return
	}

	params := &operations.UploadBackupParams{
		InstanceName: name,
		BackupID:     backupID,
	}

	var uploadResult *operations.UploadBackupResult
	op := &uploadBackupOp{
		params: params,
		deps:   s.opDeps,
		result: &uploadResult,
	}

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(uploadResult)
}

// handleRemoveLocalBackup handles DELETE /api/rdbms/{name}/backup/{id}/local
func (s *Server) handleRemoveLocalBackup(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	backupIDStr, ok := s.extractPathParam(w, r, "id")
	if !ok {
		return
	}

	backupID := 0
	if _, err := fmt.Sscanf(backupIDStr, "%d", &backupID); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid backup ID")
		return
	}

	params := operations.RemoveLocalBackupParams{
		InstanceName: name,
		BackupID:     backupID,
	}

	var result *operations.RemoveLocalBackupResult
	op := &removeLocalBackupOp{
		params: params,
		deps:   s.opDeps,
		result: &result,
	}

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// handleDownloadBackup handles POST /api/rdbms/{name}/backup/{id}/download
func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	backupIDStr, ok := s.extractPathParam(w, r, "id")
	if !ok {
		return
	}

	backupID := 0
	if _, err := fmt.Sscanf(backupIDStr, "%d", &backupID); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid backup ID")
		return
	}

	params := &operations.DownloadBackupParams{
		InstanceName: name,
		BackupID:     backupID,
	}

	var downloadResult *operations.DownloadBackupResult
	op := &downloadBackupOp{
		params: params,
		deps:   s.opDeps,
		result: &downloadResult,
	}

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(downloadResult)
}

// handleRemoveRemoteBackup handles DELETE /api/rdbms/{name}/backup/{id}/remote
func (s *Server) handleRemoveRemoteBackup(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	backupIDStr, ok := s.extractPathParam(w, r, "id")
	if !ok {
		return
	}

	backupID := 0
	if _, err := fmt.Sscanf(backupIDStr, "%d", &backupID); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid backup ID")
		return
	}

	params := operations.RemoveRemoteBackupParams{
		InstanceName: name,
		BackupID:     backupID,
	}

	var result *operations.RemoveRemoteBackupResult
	op := &removeRemoteBackupOp{
		params: params,
		deps:   s.opDeps,
		result: &result,
	}

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// listBackupsOp implements the Operation interface for listing backups
type listBackupsOp struct {
	params operations.ListBackupsParams
	deps   *operations.Dependencies
	result **operations.ListBackupsResult
}

func (op *listBackupsOp) Name() string {
	return "ListBackups"
}

func (op *listBackupsOp) Type() operations.OpType {
	return operations.OpTypeRead
}

func (op *listBackupsOp) Execute(ctx context.Context) error {
	result, err := operations.ListBackups(ctx, op.deps, op.params)
	if err != nil {
		return err
	}
	*op.result = result
	return nil
}

// uploadBackupOp implements the Operation interface for uploading backups
type uploadBackupOp struct {
	params *operations.UploadBackupParams
	deps   *operations.Dependencies
	result **operations.UploadBackupResult
}

func (op *uploadBackupOp) Name() string {
	return "UploadBackup"
}

func (op *uploadBackupOp) Type() operations.OpType {
	return operations.OpTypeWrite
}

func (op *uploadBackupOp) Execute(ctx context.Context) error {
	result, err := operations.UploadBackup(ctx, op.deps, *op.params)
	if err != nil {
		return err
	}
	*op.result = result
	return nil
}

// removeLocalBackupOp implements the Operation interface for removing local backups
type removeLocalBackupOp struct {
	params operations.RemoveLocalBackupParams
	deps   *operations.Dependencies
	result **operations.RemoveLocalBackupResult
}

func (op *removeLocalBackupOp) Name() string {
	return fmt.Sprintf("RemoveLocalBackup[%s:%d]", op.params.InstanceName, op.params.BackupID)
}

func (op *removeLocalBackupOp) Type() operations.OpType {
	return operations.OpTypeWrite
}

func (op *removeLocalBackupOp) Execute(ctx context.Context) error {
	result, err := operations.RemoveLocalBackup(ctx, op.deps, op.params)
	if err != nil {
		return err
	}
	*op.result = result
	return nil
}

// removeRemoteBackupOp implements the Operation interface for removing remote backups
type removeRemoteBackupOp struct {
	params operations.RemoveRemoteBackupParams
	deps   *operations.Dependencies
	result **operations.RemoveRemoteBackupResult
}

func (op *removeRemoteBackupOp) Name() string {
	return fmt.Sprintf("RemoveRemoteBackup[%s:%d]", op.params.InstanceName, op.params.BackupID)
}

func (op *removeRemoteBackupOp) Type() operations.OpType {
	return operations.OpTypeWrite
}

func (op *removeRemoteBackupOp) Execute(ctx context.Context) error {
	result, err := operations.RemoveRemoteBackup(ctx, op.deps, op.params)
	if err != nil {
		return err
	}
	*op.result = result
	return nil
}

// downloadBackupOp implements the Operation interface for downloading backups
type downloadBackupOp struct {
	params *operations.DownloadBackupParams
	deps   *operations.Dependencies
	result **operations.DownloadBackupResult
}

func (op *downloadBackupOp) Name() string {
	return fmt.Sprintf("DownloadBackup[%s:%d]", op.params.InstanceName, op.params.BackupID)
}

func (op *downloadBackupOp) Type() operations.OpType {
	return operations.OpTypeWrite
}

func (op *downloadBackupOp) Execute(ctx context.Context) error {
	result, err := operations.DownloadBackup(ctx, op.deps, *op.params)
	if err != nil {
		return err
	}
	*op.result = result
	return nil
}
