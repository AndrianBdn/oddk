package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/andrianbdn/oddk/internal/operations"
	s3service "github.com/andrianbdn/oddk/internal/services/s3"
)

// handleSnapshotMake handles POST /api/snapshot
//
// Snapshotting dumps every instance in turn through ephemeral helper
// containers, so it runs through the sequential executor like any other write
// operation — it must not interleave with an instance being created, destroyed
// or upgraded underneath it.
func (s *Server) handleSnapshotMake(w http.ResponseWriter, r *http.Request) {
	// A snapshot dumps every database in the deployment; on anything
	// non-trivial that comfortably exceeds the 30s WriteTimeout, which would
	// fail the response for a snapshot that actually succeeded.
	s.clearWriteDeadline(w, "snapshot make")

	// A POST with no body is normal here (comment and format are optional),
	// but a body that FAILS to parse must be a 400, not silently treated as
	// empty: it may have carried "format":"logical", and producing a physical
	// archive against an explicit request is the one substitution this
	// endpoint must never make.
	var req struct {
		Comment string `json:"comment"`
		Format  string `json:"format"`
	}
	body, readErr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if readErr != nil {
		s.writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	// Empty means the default (physical); an unknown value is a client bug and
	// must not silently produce some other format's archive.
	format, err := operations.NormalizeSnapshotFormat(req.Format)
	if err != nil {
		s.writeOpError(w, err)
		return
	}

	var result *operations.MakeSnapshotResult
	op := &snapshotMakeOp{
		params: &operations.MakeSnapshotParams{BackupDir: s.backupDir, Comment: req.Comment, Format: format},
		deps:   s.opDeps,
		result: &result,
	}

	// context.Background(), not r.Context(): a snapshot can run for a long time
	// on a large deployment, and a disconnected CLI must not abort it midway
	// and leave a partial staging tree behind. See the operations layer notes.
	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

// snapshotMakeOp implements the Operation interface for snapshot creation.
type snapshotMakeOp struct {
	params *operations.MakeSnapshotParams
	deps   *operations.Dependencies
	result **operations.MakeSnapshotResult
}

func (op *snapshotMakeOp) Name() string {
	return "MakeSnapshot"
}

func (op *snapshotMakeOp) Type() operations.OpType {
	return operations.OpTypeWrite
}

func (op *snapshotMakeOp) Execute(ctx context.Context) error {
	result, err := operations.MakeSnapshot(ctx, op.deps, op.params)
	if err != nil {
		return err
	}
	*op.result = result
	return nil
}

// AWSCredentialsBody carries CLIENT-resolved static AWS credentials for a
// one-shot S3 fetch. Precedent for secrets in a request body:
// PUT /api/offsite/config has always carried secretAccessKey the same way,
// over the same loopback-by-default channel.
//
// NEVER log this struct, its enclosing request, or a resolved aws.Credentials
// value. The daemon uses the triple only to build an in-memory S3 client;
// nothing persists it.
type AWSCredentialsBody struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`

	// SessionToken is required for STS/SSO-derived credentials (an instance
	// role resolved on the client, an assumed role, an SSO session) — they do
	// not authenticate without it.
	SessionToken string `json:"sessionToken,omitempty"`

	// Source is the chain rung that produced the triple (e.g.
	// "EnvConfigCredentials"), for the daemon's log line only.
	Source string `json:"source,omitempty"`
}

// SnapshotRestoreInstanceRequest is the body of POST /api/snapshot/restore-instance.
// Exactly one of FilePath, SnapshotID or S3URI selects the archive.
type SnapshotRestoreInstanceRequest struct {
	Instance string `json:"instance"`

	// FilePath is a path on the DAEMON's filesystem (the original form).
	FilePath string `json:"filePath,omitempty"`

	// SnapshotID names a row in this host's snapshot catalogue; the daemon
	// downloads it from S3 first (via offsite settings) if the local copy is
	// gone.
	SnapshotID int `json:"snapshotId,omitempty"`

	// S3URI names an s3://bucket/key archive, possibly from another
	// deployment. Region, Endpoint and Credentials apply only to this mode.
	S3URI       string              `json:"s3Uri,omitempty"`
	Region      string              `json:"region,omitempty"`
	Endpoint    string              `json:"endpoint,omitempty"`
	Credentials *AWSCredentialsBody `json:"credentials,omitempty"`

	// MasterKeyPath is the SOURCE host's master.key, needed only when the
	// snapshot came from a different deployment. Empty means this host's key.
	MasterKeyPath string `json:"masterKeyPath,omitempty"`
}

// handleSnapshotRestoreInstance handles POST /api/snapshot/restore-instance
//
// Unlike `snapshot apply` — which rebuilds a whole host, runs daemon-less and
// demands an empty deployment — this restores ONE instance into a deployment
// that stays up. That is exactly why it is an endpoint: it needs the executor's
// serialization and the health-check pause, both of which live in the daemon.
func (s *Server) handleSnapshotRestoreInstance(w http.ResponseWriter, r *http.Request) {
	var req SnapshotRestoreInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Instance == "" {
		s.writeError(w, http.StatusBadRequest, "instance is required")
		return
	}
	sources := 0
	if req.FilePath != "" {
		sources++
	}
	if req.SnapshotID != 0 {
		sources++
	}
	if req.S3URI != "" {
		sources++
	}
	if sources != 1 {
		s.writeError(w, http.StatusBadRequest, "exactly one of filePath, snapshotId or s3Uri is required")
		return
	}
	if req.SnapshotID < 0 {
		s.writeError(w, http.StatusBadRequest, "invalid snapshot id")
		return
	}
	if req.S3URI == "" && (req.Region != "" || req.Endpoint != "" || req.Credentials != nil) {
		s.writeError(w, http.StatusBadRequest, "region, endpoint and credentials are only meaningful with s3Uri")
		return
	}

	src := &operations.RestoreArchiveSource{
		ArchivePath: req.FilePath,
		SnapshotID:  req.SnapshotID,
	}
	if req.S3URI != "" {
		spec := &operations.RemoteSnapshotSpec{
			URI:      req.S3URI,
			Region:   req.Region,
			Endpoint: req.Endpoint,
		}
		if req.Credentials != nil {
			spec.Credentials = &s3service.StaticCredentials{
				AccessKeyID:     req.Credentials.AccessKeyID,
				SecretAccessKey: req.Credentials.SecretAccessKey,
				SessionToken:    req.Credentials.SessionToken,
			}
		}
		src.Remote = spec
	}

	// Extracting an archive and replaying every database in an instance runs far
	// past the 30s WriteTimeout — and an S3 fetch may come first.
	s.clearWriteDeadline(w, fmt.Sprintf("snapshot restore-instance %s", req.Instance))

	// The cluster is torn down and rebuilt underneath the health checker, and
	// its cached connection must not outlive the container it points at.
	s.pauseHealthChecksAndCleanupConnections(req.Instance)
	defer s.unpauseHealthChecks()

	var result *operations.RestoreInstanceResult
	op := &snapshotRestoreInstanceOp{
		src: src,
		params: &operations.RestoreInstanceParams{
			InstanceName:  req.Instance,
			MasterKeyPath: req.MasterKeyPath,
			BackupDir:     s.backupDir,
		},
		deps:   s.opDeps,
		result: &result,
	}

	// context.Background(), not r.Context(): aborting midway would leave the
	// instance with a destroyed cluster and a partial restore.
	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}

	s.writeJSON(w, http.StatusOK, result)
}

// snapshotRestoreInstanceOp implements the Operation interface for restoring a
// single instance out of a snapshot. Resolving the archive source (which may
// download from S3) runs INSIDE the executor, so a fetch serializes with other
// operations exactly like download-by-id always has.
type snapshotRestoreInstanceOp struct {
	src    *operations.RestoreArchiveSource
	params *operations.RestoreInstanceParams
	deps   *operations.Dependencies
	result **operations.RestoreInstanceResult
}

func (op *snapshotRestoreInstanceOp) Name() string {
	return "RestoreInstanceFromSnapshot"
}

func (op *snapshotRestoreInstanceOp) Type() operations.OpType {
	return operations.OpTypeWrite
}

func (op *snapshotRestoreInstanceOp) Execute(ctx context.Context) error {
	archivePath, foreign, origin, err := operations.ResolveRestoreInstanceArchive(
		ctx, op.deps, op.src, op.params.BackupDir, op.params.Progress)
	if err != nil {
		return err
	}
	op.params.ArchivePath = archivePath
	op.params.ForeignSource = foreign

	result, err := operations.RestoreInstanceFromSnapshot(ctx, op.deps, op.params)
	if err != nil {
		return err
	}
	result.ArchiveOrigin = origin
	*op.result = result
	return nil
}

// handleSnapshotListRemote handles GET /api/snapshots/remote
//
// It reports the BUCKET's truth under the ODDK snapshot layout — including
// archives whose catalogue rows this host no longer has (another host's
// snapshots in a shared bucket, rows lost with a dead host) — which is exactly
// what `snapshot list` cannot show. Read-only, so it skips the executor like
// the catalogue list does.
func (s *Server) handleSnapshotListRemote(w http.ResponseWriter, r *http.Request) {
	settings, err := operations.GetActiveOffsiteSettingsDecrypted(s.opDeps)
	if err != nil {
		s.writeOpError(w, err)
		return
	}
	if settings == nil {
		s.writeError(w, http.StatusBadRequest,
			"offsite backup not configured; pass an explicit s3:// URI to list a bucket with your own AWS credentials")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	client, err := s3service.NewClient(ctx, settings)
	if err != nil {
		s.writeOpError(w, fmt.Errorf("create S3 client: %w", err))
		return
	}

	prefix := operations.SnapshotS3Prefix + "/"
	objects, truncated, err := operations.ListRemoteSnapshots(ctx, client, settings.Bucket, prefix)
	if err != nil {
		s.writeOpError(w, err)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"bucket":    settings.Bucket,
		"prefix":    client.GetBucketPath() + prefix,
		"objects":   objects,
		"truncated": truncated,
	})
}

// SnapshotCronRequest is the body of POST /api/cron/snapshot.
type SnapshotCronRequest struct {
	UTCHour           int `json:"utcHour"`
	IntervalHours     int `json:"intervalHours"`
	CleanupLocalDays  int `json:"cleanupLocalDays"`
	CleanupRemoteDays int `json:"cleanupRemoteDays"`

	// Format is "physical" or "logical". Empty follows the merge rule below:
	// an existing plan keeps its format, a new plan defaults to physical.
	Format string `json:"format,omitempty"`
}

// validSnapshotIntervals are the divisors of 24.
//
// An interval that does not divide 24 goes ragged across midnight — 5 hours
// anchored at 03 fires 03,08,13,18,23 and then 03, a 4-hour gap that silently
// breaks the schedule the operator asked for. The same set is enforced by a
// CHECK in migration 017; this exists to reject it with an explanation instead
// of a constraint error.
var validSnapshotIntervals = []int{1, 2, 3, 4, 6, 8, 12, 24}

func (s *Server) handleCronSnapshotSet(w http.ResponseWriter, r *http.Request) {
	var req SnapshotCronRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.UTCHour < 0 || req.UTCHour > 23 {
		s.writeError(w, http.StatusBadRequest, "utcHour must be between 0 and 23")
		return
	}
	// Absent fields MERGE with the existing plan rather than snapping back to
	// defaults. Otherwise `setup-cron --utc-hour 4` on a plan configured to run
	// every 6 hours silently reverts it to daily — the operator asked to move
	// the anchor, not to change the frequency.
	existing, err := s.store.Snapshot.GetPlan()
	if err != nil {
		s.writeOpError(w, err)
		return
	}
	defaults := struct {
		interval, local, remote int
		format                  string
	}{24, 7, 14, operations.SnapshotFormatPhysical}
	if existing != nil {
		defaults.interval = existing.IntervalHours
		defaults.local = existing.CleanupLocalDays
		defaults.remote = existing.CleanupRemoteDays
		if existing.Format != "" {
			defaults.format = existing.Format
		}
	}
	if req.IntervalHours == 0 {
		req.IntervalHours = defaults.interval
	}
	if req.Format == "" {
		req.Format = defaults.format
	}
	format, err := operations.NormalizeSnapshotFormat(req.Format)
	if err != nil {
		s.writeOpError(w, err)
		return
	}
	if !slices.Contains(validSnapshotIntervals, req.IntervalHours) {
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"intervalHours must divide 24 evenly (one of %v); anything else drifts across midnight and would not honour the interval you asked for",
			validSnapshotIntervals))
		return
	}
	if req.CleanupLocalDays == 0 {
		req.CleanupLocalDays = defaults.local
	}
	if req.CleanupRemoteDays == 0 {
		req.CleanupRemoteDays = defaults.remote
	}
	if req.CleanupLocalDays < 1 || req.CleanupRemoteDays < 1 {
		s.writeError(w, http.StatusBadRequest, "cleanup day counts must be at least 1")
		return
	}

	if err := s.store.Snapshot.SetPlan(req.UTCHour, req.IntervalHours, req.CleanupLocalDays, req.CleanupRemoteDays, format); err != nil {
		s.writeOpError(w, err)
		return
	}
	plan, err := s.store.Snapshot.GetPlan()
	if err != nil {
		s.writeOpError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleCronSnapshotGet(w http.ResponseWriter, r *http.Request) {
	plan, err := s.store.Snapshot.GetPlan()
	if err != nil {
		s.writeOpError(w, err)
		return
	}
	// A missing plan is an ordinary state, not a 404: the client asks in order
	// to render "not configured".
	s.writeJSON(w, http.StatusOK, map[string]any{"plan": plan})
}

func (s *Server) handleCronSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Snapshot.DeletePlan(); err != nil {
		s.writeOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSnapshotList(w http.ResponseWriter, r *http.Request) {
	records, err := s.store.Snapshot.List()
	if err != nil {
		s.writeOpError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, records)
}

func (s *Server) handleSnapshotUpload(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		s.writeError(w, http.StatusBadRequest, "invalid snapshot id")
		return
	}

	// A whole-deployment archive can take far longer than the 30s WriteTimeout
	// to push to S3 — the per-instance upload handler's failure to do this is a
	// known wart, not a precedent to copy.
	s.clearWriteDeadline(w, fmt.Sprintf("snapshot upload %d", id))

	var result *operations.UploadSnapshotResult
	op := &snapshotUploadOp{deps: s.opDeps, id: id, result: &result}
	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

// handleSnapshotDownload handles POST /api/snapshot/{id}/download
//
// The counterpart to upload. Without it an offsite snapshot is write-only: local
// retention removes the local copy and the primary DR artifact can no longer be
// obtained through ODDK.
func (s *Server) handleSnapshotDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := s.snapshotIDParam(w, r)
	if !ok {
		return
	}
	s.clearWriteDeadline(w, fmt.Sprintf("snapshot download %d", id))

	var result *operations.DownloadSnapshotResult
	op := &snapshotFuncOp{
		name: "DownloadSnapshot",
		run: func(ctx context.Context) error {
			res, err := operations.DownloadSnapshot(ctx, s.opDeps, id, s.backupDir)
			if err != nil {
				return err
			}
			result = res
			return nil
		},
	}
	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

// handleSnapshotRemoveLocal handles DELETE /api/snapshot/{id}/local
func (s *Server) handleSnapshotRemoveLocal(w http.ResponseWriter, r *http.Request) {
	id, ok := s.snapshotIDParam(w, r)
	if !ok {
		return
	}
	op := &snapshotFuncOp{
		name: "RemoveSnapshotLocal",
		run:  func(ctx context.Context) error { return operations.RemoveSnapshotLocal(ctx, s.opDeps, id) },
	}
	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSnapshotRemoveRemote handles DELETE /api/snapshot/{id}/remote
func (s *Server) handleSnapshotRemoveRemote(w http.ResponseWriter, r *http.Request) {
	id, ok := s.snapshotIDParam(w, r)
	if !ok {
		return
	}
	op := &snapshotFuncOp{
		name: "RemoveSnapshotRemote",
		run:  func(ctx context.Context) error { return operations.RemoveSnapshotRemote(ctx, s.opDeps, id) },
	}
	if err := s.executor.Execute(context.Background(), op); err != nil {
		s.writeOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) snapshotIDParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		s.writeError(w, http.StatusBadRequest, "invalid snapshot id")
		return 0, false
	}
	return id, true
}

// snapshotFuncOp adapts a plain function to the Operation interface, for the
// short snapshot operations that do not need their own type.
type snapshotFuncOp struct {
	name string
	run  func(ctx context.Context) error
}

func (op *snapshotFuncOp) Name() string                      { return op.name }
func (op *snapshotFuncOp) Type() operations.OpType           { return operations.OpTypeWrite }
func (op *snapshotFuncOp) Execute(ctx context.Context) error { return op.run(ctx) }

type snapshotUploadOp struct {
	deps   *operations.Dependencies
	id     int
	result **operations.UploadSnapshotResult
}

func (op *snapshotUploadOp) Name() string { return "UploadSnapshot" }

func (op *snapshotUploadOp) Type() operations.OpType { return operations.OpTypeWrite }

func (op *snapshotUploadOp) Execute(ctx context.Context) error {
	result, err := operations.UploadSnapshot(ctx, op.deps, op.id)
	if err != nil {
		return err
	}
	*op.result = result
	return nil
}
