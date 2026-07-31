package operations

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/rfc3339time"
	s3service "github.com/andrianbdn/oddk/internal/services/s3"
	"github.com/andrianbdn/oddk/internal/store/offsite"
)

// maxPutObjectBytes is S3's hard limit for a single PutObject.
//
// s3.Client.UploadFile is a single PutObject — there is no multipart upload and
// aws-sdk-go-v2's feature/s3/manager is deliberately not a dependency. That has
// never mattered for per-instance backups, but a snapshot covers the WHOLE
// deployment and can plausibly cross 5 GiB. Checking up front turns an opaque
// SDK failure at the end of a long upload into an actionable refusal.
const maxPutObjectBytes int64 = 5 * 1024 * 1024 * 1024

// snapshotS3Prefix is the first key segment for snapshots, mirroring how a
// backup's key starts with its instance name. Snapshots belong to no single
// instance, and "*" cannot collide with a real one because instance names are
// restricted to letters, digits, '-' and '_'.
const snapshotS3Prefix = "*snapshots*"

// UploadSnapshotResult describes a completed offsite upload.
type UploadSnapshotResult struct {
	ID             int    `json:"id"`
	Filename       string `json:"filename"`
	Size           int64  `json:"size"`
	RemoteLocation string `json:"remoteLocation"`
}

// UploadSnapshot ships one snapshot archive offsite.
//
// Mirrors UploadBackup: same client, same log table, same s3:// location format
// (prefix baked in, which is why RelativeKey exists on the read side).
func UploadSnapshot(ctx context.Context, deps *Dependencies, id int) (*UploadSnapshotResult, error) {
	settings, err := GetActiveOffsiteSettingsDecrypted(deps)
	if err != nil {
		return nil, fmt.Errorf("get offsite settings: %w", err)
	}
	if settings == nil {
		return nil, operr.Invalidf("offsite backup not configured")
	}

	record, err := deps.Store.Snapshot.Get(id)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, operr.NotFoundf("snapshot %d not found", id)
	}
	if record.RemotePath != "" {
		return nil, operr.Conflictf("snapshot %d is already uploaded: %s", id, record.RemotePath)
	}
	if record.LocalPath == "" {
		return nil, operr.Invalidf("snapshot %d has no local file to upload", id)
	}
	if !filepath.IsAbs(record.LocalPath) {
		return nil, operr.Invalidf("snapshot location is not an absolute path: %s", record.LocalPath)
	}

	localFile, err := os.Open(record.LocalPath) // #nosec G304 - path comes from our own catalogue
	if err != nil {
		return nil, fmt.Errorf("open snapshot file: %w", err)
	}
	defer func() { _ = localFile.Close() }()

	info, err := localFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat snapshot file: %w", err)
	}
	if info.Size() > maxPutObjectBytes {
		return nil, operr.Invalidf(
			"snapshot %d is %.1f GiB, above the %d GiB limit for a single S3 PutObject, and ODDK does not do multipart uploads. Copy it offsite by other means, or reduce what the deployment holds",
			id, float64(info.Size())/(1024*1024*1024), maxPutObjectBytes/(1024*1024*1024))
	}

	s3Client, err := s3service.NewClient(ctx, settings)
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}

	filename := filepath.Base(record.LocalPath)
	// Key off the snapshot's OWN creation date, not the upload time.
	//
	// The per-instance backup path uses time.Now(), which makes a retry land on
	// a different key from the attempt that failed: if the object was actually
	// written and only the bookkeeping failed, the first object is orphaned in
	// S3 forever, invisible to ODDK and to remote retention (which works off the
	// recorded location). Deriving the key from CreatedAt makes upload
	// idempotent — a retry targets the same key and overwrites it.
	s3Key := fmt.Sprintf("%s/%s/%s", snapshotS3Prefix, record.CreatedAt.Format("2006-01-02"), filename)

	exists, err := s3Client.FileExists(ctx, s3Key)
	if err != nil {
		return nil, fmt.Errorf("check S3 file existence: %w", err)
	}
	if exists {
		if err := s3Client.DeleteFile(ctx, s3Key); err != nil {
			return nil, fmt.Errorf("delete existing S3 object: %w", err)
		}
	}

	if _, err := localFile.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("reset file position: %w", err)
	}
	if uploadErr := s3Client.UploadFile(ctx, s3Key, localFile); uploadErr != nil {
		// Unlike UploadBackup, record the failure too. A snapshot upload is the
		// offsite leg of disaster recovery, so "it silently never happened" is
		// exactly the state an audit needs to be able to see.
		logOffsiteFailure(deps, settings, "snapshot_upload",
			fmt.Sprintf("%s -> s3://%s/%s", filename, settings.Bucket, s3Key), uploadErr)
		return nil, fmt.Errorf("upload to S3: %w", uploadErr)
	}

	exists, err = s3Client.FileExists(ctx, s3Key)
	if err != nil {
		return nil, fmt.Errorf("verify upload: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("upload verification failed: snapshot not found in S3 after upload")
	}

	s3Location := fmt.Sprintf("s3://%s/%s%s", settings.Bucket, s3Client.GetBucketPath(), s3Key)
	if err := deps.Store.Snapshot.SetRemoteLocation(id, s3Location); err != nil {
		return nil, err
	}

	if err := deps.Store.Offsite.AddLog(&offsite.OffsiteLog{
		Event:             "snapshot_upload",
		OffsiteSettingsID: settings.ID,
		Object:            fmt.Sprintf("%s -> s3://%s/%s", filename, settings.Bucket, s3Key),
		Success:           true,
		CreatedAt:         rfc3339time.Now(),
	}); err != nil {
		// The object is in S3 and the location is recorded; a missing log line
		// must not fail the upload.
		log.Printf("WARNING: snapshot %d uploaded but the offsite log row failed: %v", id, err)
	}

	return &UploadSnapshotResult{
		ID:             id,
		Filename:       filename,
		Size:           info.Size(),
		RemoteLocation: s3Location,
	}, nil
}

// logOffsiteFailure appends a failed offsite event, best-effort.
func logOffsiteFailure(deps *Dependencies, settings *offsite.OffsiteSettings, event, object string, cause error) {
	detail := cause.Error()
	if err := deps.Store.Offsite.AddLog(&offsite.OffsiteLog{
		Event:             event,
		OffsiteSettingsID: settings.ID,
		Object:            object,
		Success:           false,
		ErrorDetails:      &detail,
		CreatedAt:         rfc3339time.Now(),
	}); err != nil {
		log.Printf("WARNING: could not record offsite failure for %s: %v", object, err)
	}
}
