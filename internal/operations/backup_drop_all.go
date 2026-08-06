package operations

import (
	"context"
	"fmt"
	"os"

	s3service "github.com/andrianbdn/oddk/internal/services/s3"
)

// DropAllBackupsResult reports what DropAllBackups did. Errors are collected
// per record rather than aborting the sweep: the command exists to
// decommission the legacy per-instance backup system, and dying on record 3
// of 200 would leave the operator guessing what remains. A record whose
// copies could not all be dealt with is KEPT, so the run is re-entrant.
type DropAllBackupsResult struct {
	RecordsTotal               int      `json:"recordsTotal"`
	RecordsDropped             int      `json:"recordsDropped"`
	RecordsKept                int      `json:"recordsKept"`
	LocalFilesDeleted          int      `json:"localFilesDeleted"`
	LocalFilesMissing          int      `json:"localFilesMissing"`
	LocalBytesFreed            int64    `json:"localBytesFreed"`
	RemoteObjectsDeleted       int      `json:"remoteObjectsDeleted"`
	RemoteRefsDroppedNoOffsite int      `json:"remoteRefsDroppedNoOffsite"`
	Errors                     []string `json:"errors"`
	Message                    string   `json:"message"`
}

// DropAllBackups deletes EVERY per-instance backup: local archive files, S3
// objects, and the backup_history records — across all instances, including
// records whose instance has since been destroyed (which is why this cannot
// be a client-side loop over the per-ID remove endpoints: those refuse when
// the instance no longer exists). Snapshot archives are untouched — this
// walks backup_history only.
//
// Copy-handling rules, chosen so a partial run can always be re-run:
//   - A local file that is already gone is fine (the record is still dropped);
//     any other unlink failure keeps the record and is reported.
//   - With offsite configured, an S3 delete failure keeps the record (unlike
//     per-ID remove-remote's warn-and-clear): silently mass-orphaning objects
//     in the bucket is exactly what a bulk sweep must not do. If the local
//     copy was already removed, the record's local location is cleared so it
//     reflects reality (the CHECK constraint holds — remote is still set).
//   - With offsite NOT configured, remote references are dropped and counted:
//     ODDK has no way to reach those objects, and keeping the records forever
//     would make the sweep unfinishable. Mirrors remove-remote's semantics;
//     the caller is told the objects themselves survive in the bucket.
func DropAllBackups(ctx context.Context, deps *Dependencies) (*DropAllBackupsResult, error) {
	records, err := deps.Store.Backup.AllRecordsRaw()
	if err != nil {
		return nil, fmt.Errorf("list backup records: %w", err)
	}

	result := &DropAllBackupsResult{
		RecordsTotal: len(records),
		Errors:       []string{},
	}
	if len(records) == 0 {
		result.Message = "No backup records found — nothing to do"
		return result, nil
	}

	settings, err := GetActiveOffsiteSettingsDecrypted(deps)
	if err != nil {
		return nil, fmt.Errorf("get offsite settings: %w", err)
	}

	// Create the S3 client up front, before anything is deleted: with offsite
	// configured but unusable, aborting cleanly beats keeping every
	// remote-holding record and reporting a wall of identical errors.
	var s3Client *s3service.Client
	if settings != nil {
		for _, rec := range records {
			if rec.RemoteLocation.Valid {
				s3Client, err = s3service.NewClient(ctx, settings)
				if err != nil {
					return nil, fmt.Errorf("create S3 client (offsite is configured and some backups have remote copies): %w", err)
				}
				break
			}
		}
	}

	for _, rec := range records {
		fail := func(op string, err error) {
			result.RecordsKept++
			result.Errors = append(result.Errors,
				fmt.Sprintf("backup %d (%s): %s: %v", rec.ID, rec.InstanceName, op, err))
			deps.Logger.Printf("Warning: drop-all kept backup %d (%s): %s: %v",
				rec.ID, rec.InstanceName, op, err)
		}

		localCleared := false
		if rec.LocalLocation.Valid {
			path := deps.Store.Backup.AbsoluteLocalPath(rec.LocalLocation.String)
			var size int64
			if info, statErr := os.Stat(path); statErr == nil {
				size = info.Size()
			}
			switch rmErr := os.Remove(path); {
			case rmErr == nil:
				result.LocalFilesDeleted++
				result.LocalBytesFreed += size
				localCleared = true
			case os.IsNotExist(rmErr):
				result.LocalFilesMissing++
				localCleared = true
			default:
				// Leave the record fully intact — the remote copy (if any) is
				// deliberately not touched either, so the record keeps
				// describing exactly what still exists.
				fail("delete local file", rmErr)
				continue
			}
		}

		if rec.RemoteLocation.Valid {
			if settings == nil {
				result.RemoteRefsDroppedNoOffsite++
			} else {
				keepRemoteRecord := func(op string, err error) {
					fail(op, err)
					// The local file is already gone; make the kept record say so.
					if localCleared && rec.LocalLocation.Valid {
						if clearErr := deps.Store.Backup.ClearLocalLocation(rec.ID); clearErr != nil {
							deps.Logger.Printf("Warning: drop-all could not clear local location of backup %d: %v",
								rec.ID, clearErr)
						}
					}
				}
				s3Key, parseErr := parseRemoteS3Location(rec.RemoteLocation.String, settings.Bucket)
				if parseErr != nil {
					keepRemoteRecord("parse remote location", parseErr)
					continue
				}
				if delErr := s3Client.DeleteFile(ctx, s3Client.RelativeKey(s3Key)); delErr != nil {
					keepRemoteRecord("delete S3 object", delErr)
					continue
				}
				result.RemoteObjectsDeleted++
			}
		}

		if err := deps.Store.Backup.DeleteRecord(rec.ID); err != nil {
			fail("delete record", err)
			continue
		}
		result.RecordsDropped++
	}

	result.Message = fmt.Sprintf("Dropped %d of %d backup record(s)", result.RecordsDropped, result.RecordsTotal)
	if result.RecordsKept > 0 {
		result.Message += fmt.Sprintf("; %d kept because a copy could not be removed — fix the cause and re-run", result.RecordsKept)
	}
	return result, nil
}
