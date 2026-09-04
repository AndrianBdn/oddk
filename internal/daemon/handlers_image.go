package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/moby/moby/api/types/jsonstream"

	"github.com/andrianbdn/oddk/internal/operations"
)

// Request types for image operations
type PullImageRequest struct {
	Version string `json:"version"`
	Image   string `json:"image"`
	// IfMissing pulls only when the image is not already present locally.
	IfMissing bool `json:"ifMissing"`
}

// handlePullImage handles POST /api/pull.
//
// The response is a stream of newline-delimited Docker progress frames
// (application/x-ndjson) so the CLI can render live progress. Validation errors
// that are knowable before the pull starts are returned as a normal JSON 4xx;
// once streaming has begun the status is necessarily 200 and any failure is
// reported as a jsonmessage error frame inside the stream.
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

	op := operations.NewPullImageOp(s.opDeps, operations.PullImageParams{
		Version:   req.Version,
		Image:     req.Image,
		IfMissing: req.IfMissing,
	})

	// Resolve + validate before committing to a streaming response so a bad
	// version/image combination still surfaces as a clean 4xx.
	if err := op.Resolve(); err != nil {
		s.writeOpError(w, fmt.Errorf("pull image: %w", err))
		return
	}

	// A large/slow pull can stream for longer than the server's WriteTimeout;
	// without clearing the deadline the connection is cut mid-download and the
	// client sees "unexpected EOF".
	s.clearWriteDeadline(w, "pull image")

	flusher := s.beginProgressStream(w)
	op.SetProgressWriter(&flushWriter{w: w, flusher: flusher})

	// Uninterruptible by client disconnect, like other write operations.
	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeStreamError(w, flusher, "pull image", err)
	}
}

// UpdateRequest is the body for POST /api/rdbms/{name}/update. Image is
// optional (defaults to the instance's current image).
type UpdateRequest struct {
	Image string `json:"image"`
}

// handleUpdateRDBMS handles POST /api/rdbms/{name}/update. It re-pulls the
// instance's image tag and recreates the container if a newer patch was
// fetched, streaming Docker progress like POST /api/pull.
func (s *Server) handleUpdateRDBMS(w http.ResponseWriter, r *http.Request) {
	name, ok := s.extractPathParam(w, r, "name")
	if !ok {
		return
	}

	var req UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Pre-check existence so a missing instance is a clean 404 rather than an
	// in-stream error.
	if _, err := s.opDeps.Store.Instances.Get(name); err != nil {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("instance %s not found", name))
		return
	}

	// Recreate waits for readiness, which can exceed WriteTimeout.
	s.clearWriteDeadline(w, fmt.Sprintf("update %s", name))
	s.pauseHealthChecksAndCleanupConnections(name)
	defer s.unpauseHealthChecks()

	op := operations.NewUpdateRDBMSOp(s.opDeps, operations.UpdateRDBMSParams{
		Name:  name,
		Image: req.Image,
	})

	flusher := s.beginProgressStream(w)
	op.SetProgressWriter(&flushWriter{w: w, flusher: flusher})

	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeStreamError(w, flusher, "update instance", err)
	}
}

// beginProgressStream switches the response into newline-delimited Docker
// progress streaming mode (application/x-ndjson) and returns the flusher (nil
// if the writer can't flush).
func (s *Server) beginProgressStream(w http.ResponseWriter) http.Flusher {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	return flusher
}

// writeStreamError reports an operation failure once streaming has begun: the
// 200 header is already sent, so the error goes inside the stream as a
// jsonmessage error frame (which the CLI renders and exits non-zero on) and is
// logged server-side.
func (s *Server) writeStreamError(w http.ResponseWriter, flusher http.Flusher, what string, err error) {
	log.Printf("%s stream error: %v", what, err)
	_ = json.NewEncoder(w).Encode(jsonstream.Message{
		Error: &jsonstream.Error{Message: err.Error()},
	})
	if flusher != nil {
		flusher.Flush()
	}
}
