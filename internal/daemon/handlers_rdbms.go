package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/hypersequent/oddk/internal/operations"
	"github.com/hypersequent/oddk/internal/store/parameters"
	"github.com/hypersequent/oddk/internal/util"
)

// clearWriteDeadline removes the response write deadline for long-running
// operations (create/start/switch/reconfigure/major-upgrade) that can run past
// the server's WriteTimeout while waiting for PostgreSQL readiness or a
// migration. Without this, a slow first-boot initdb could exceed WriteTimeout
// and make the CLI report a spurious failure for an instance that came up fine.
func (s *Server) clearWriteDeadline(w http.ResponseWriter, op string) {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		fmt.Printf("%s: could not clear write deadline: %v\n", op, err)
	}
}

// Request/Response types for RDBMS operations
type CreateRDBMSRequest struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	Image          string `json:"image"`
	Port           int    `json:"port"`
	CPUCores       int    `json:"cpuCores"`
	RAMMB          int    `json:"ramMb"`
	ParameterGroup string `json:"parameterGroup"`
}

type SwitchRequest struct {
	Image   string `json:"image"`
	Version string `json:"version"`
}

type UpgradeRequest struct {
	TargetVersion string `json:"targetVersion"`
	Image         string `json:"image"`
}

type UpdateStateRequest struct {
	State string `json:"state"`
}

type ReconfigureRequest struct {
	ParameterGroup string `json:"parameterGroup"`
}

// handleListRDBMS handles GET /api/rdbms
func (s *Server) handleListRDBMS(w http.ResponseWriter, r *http.Request) {
	op := operations.NewListRDBMSOp(s.opDeps)

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list instances: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, op.GetResult())
}

// handleGetRDBMS handles GET /api/rdbms/{name}
func (s *Server) handleGetRDBMS(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	op := operations.NewGetRDBMSOp(s.opDeps, name)

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("instance not found: %s", name))
		return
	}

	s.writeJSON(w, http.StatusOK, op.GetResult())
}

// handleCreateRDBMS handles POST /api/rdbms
func (s *Server) handleCreateRDBMS(w http.ResponseWriter, r *http.Request) {
	var req CreateRDBMSRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := util.ValidateInstanceName(req.Name); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.CPUCores == 0 {
		s.writeError(w, http.StatusBadRequest, "cpuCores is required")
		return
	}

	if req.RAMMB == 0 {
		s.writeError(w, http.StatusBadRequest, "ramMB is required")
		return
	}

	if err := util.ValidateSystemResources(req.CPUCores, req.RAMMB); err != nil {
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid resource allocation: %v", err))
		return
	}

	// Use default parameter group if not specified
	parameterGroup := req.ParameterGroup
	if parameterGroup == "" {
		parameterGroup = parameters.DefaultParameterGroup
	}

	// Create waits for PostgreSQL readiness (first boot runs initdb), which can
	// exceed the server's WriteTimeout.
	s.clearWriteDeadline(w, fmt.Sprintf("create %s", req.Name))

	// Pause health checks during instance creation to avoid failures during startup
	if s.healthScheduler != nil {
		s.healthScheduler.Pause()
		defer s.unpauseHealthChecks()
	}

	params := operations.CreateRDBMSParams{
		Name:           req.Name,
		Version:        req.Version,
		Image:          req.Image,
		Port:           req.Port,
		CPUCores:       req.CPUCores,
		RAMMB:          req.RAMMB,
		ParameterGroup: parameterGroup,
	}

	op := operations.NewCreateRDBMSOp(s.opDeps, params)

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	result := op.GetResult()
	responseInstance := *result.Instance
	responseInstance.Password = result.Password

	s.writeJSON(w, http.StatusCreated, responseInstance)
}

// handleUpdateRDBMSState handles PUT /api/rdbms/{name}/state
func (s *Server) handleUpdateRDBMSState(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	var req UpdateStateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	params := operations.UpdateStateParams{
		Name:  name,
		State: req.State,
	}

	// Starting waits for PostgreSQL readiness, which can exceed WriteTimeout.
	s.clearWriteDeadline(w, fmt.Sprintf("state %s", name))

	// If stopping the instance, coordinate connection cleanup first
	if req.State == "stop" {
		s.pauseHealthChecksAndCleanupConnections(name)
		defer s.unpauseHealthChecks()
	}

	op := operations.NewUpdateStateOp(s.opDeps, params)

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

// handleDeleteRDBMS handles DELETE /api/rdbms/{name}
func (s *Server) handleDeleteRDBMS(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	// Coordinate connection cleanup before destroying instance
	s.pauseHealthChecksAndCleanupConnections(name)
	defer s.unpauseHealthChecks()

	params := operations.DeleteRDBMSParams{
		Name: name,
	}

	op := operations.NewDeleteRDBMSOp(s.opDeps, params)

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleReconfigureRDBMS handles PUT /api/rdbms/{name}/config
func (s *Server) handleReconfigureRDBMS(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	var req ReconfigureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.ParameterGroup == "" {
		s.writeError(w, http.StatusBadRequest, "parameterGroup is required")
		return
	}

	// Reconfigure recreates the container and waits for readiness, which can
	// exceed WriteTimeout.
	s.clearWriteDeadline(w, fmt.Sprintf("reconfigure %s", name))

	// Coordinate connection cleanup before reconfiguring instance
	s.pauseHealthChecksAndCleanupConnections(name)
	defer s.unpauseHealthChecks()

	params := operations.ReconfigureRDBMSParams{
		Name:           name,
		ParameterGroup: req.ParameterGroup,
	}

	op := operations.NewReconfigureRDBMSOp(s.opDeps, params)

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	result := op.GetResult()
	s.writeJSON(w, http.StatusOK, result.Instance)
}

// handleMajorUpgradeRDBMS handles POST /api/rdbms/{name}/major-upgrade
func (s *Server) handleMajorUpgradeRDBMS(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	var req UpgradeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.TargetVersion == "" {
		s.writeError(w, http.StatusBadRequest, "targetVersion is required")
		return
	}

	// A major upgrade is a dump + fresh-cluster + restore and can run far
	// longer than the server's 30s WriteTimeout. Clear the write deadline for
	// this request so the response isn't cut off mid-upgrade.
	s.clearWriteDeadline(w, fmt.Sprintf("major-upgrade %s", name))

	// Coordinate connection cleanup before the destructive upgrade.
	s.pauseHealthChecksAndCleanupConnections(name)
	defer s.unpauseHealthChecks()

	params := operations.UpgradeRDBMSParams{
		Name:          name,
		TargetVersion: req.TargetVersion,
		Image:         req.Image,
	}

	op := operations.NewUpgradeRDBMSOp(s.opDeps, params)

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	s.writeJSON(w, http.StatusOK, op.GetResult())
}

// handleGetRDBMSLogs handles GET /api/rdbms/{name}/logs
// Query params: tail (default "100"), follow (default "false")
func (s *Server) handleGetRDBMSLogs(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	tail := r.URL.Query().Get("tail")
	if tail == "" {
		tail = "100"
	}

	params := operations.GetLogsParams{InstanceName: name, Tail: tail}

	if r.URL.Query().Get("follow") == "true" {
		// Streaming mode: write plain text, flush as Docker frames arrive
		flusher, canFlush := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		if canFlush {
			flusher.Flush()
		}

		fw := &flushWriter{w: w, flusher: flusher}
		if err := operations.StreamInstanceLogs(r.Context(), s.opDeps, params, fw); err != nil {
			// Headers already sent; log the error server-side only
			fmt.Printf("stream logs error for %s: %v\n", name, err)
		}
		return
	}

	// Non-streaming: return JSON
	result, err := operations.GetInstanceLogs(r.Context(), s.opDeps, params)
	if err != nil {
		s.writeOpError(w, fmt.Errorf("get logs: %w", err))
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"logs": result.Logs})
}

// flushWriter wraps a ResponseWriter and flushes after every write.
type flushWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (fw *flushWriter) Write(p []byte) (n int, err error) {
	n, err = fw.w.Write(p)
	if err == nil && fw.flusher != nil {
		fw.flusher.Flush()
	}
	return n, err
}

// handleSwitchRDBMS handles PUT /api/rdbms/{name}/image
func (s *Server) handleSwitchRDBMS(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	var req SwitchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Image == "" {
		s.writeError(w, http.StatusBadRequest, "image is required")
		return
	}

	// Switch recreates the container and waits for readiness, which can exceed
	// WriteTimeout.
	s.clearWriteDeadline(w, fmt.Sprintf("switch %s", name))

	// Coordinate connection cleanup before switching instance image
	s.pauseHealthChecksAndCleanupConnections(name)
	defer s.unpauseHealthChecks()

	params := operations.SwitchRDBMSParams{
		Name:    name,
		Image:   req.Image,
		Version: req.Version,
	}

	op := operations.NewSwitchRDBMSOp(s.opDeps, params)

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	result := op.GetResult()
	s.writeJSON(w, http.StatusOK, result.Instance)
}
