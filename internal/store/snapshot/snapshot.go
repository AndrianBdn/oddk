package snapshot

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/jmoiron/sqlx"

	"github.com/andrianbdn/oddk/internal/rfc3339time"
)

// planID is the only permitted primary key: snapshot_plans is a singleton,
// enforced by a CHECK in migration 017.
const planID = 1

type SnapshotStore struct {
	db *sqlx.DB
}

func NewSnapshotStore(db *sqlx.DB) *SnapshotStore {
	return &SnapshotStore{db: db}
}

// SetPlan creates or replaces the deployment's snapshot schedule, preserving
// created_at across updates (as CronStore.CreatePlan does).
func (s *SnapshotStore) SetPlan(utcHour, intervalHours, cleanupLocalDays, cleanupRemoteDays int, format string) error {
	now := rfc3339time.Now()
	_, err := s.db.Exec(`
		INSERT INTO snapshot_plans (id, utc_hour, interval_hours, cleanup_local_days, cleanup_remote_days, format, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			utc_hour = excluded.utc_hour,
			interval_hours = excluded.interval_hours,
			cleanup_local_days = excluded.cleanup_local_days,
			cleanup_remote_days = excluded.cleanup_remote_days,
			format = excluded.format,
			updated_at = excluded.updated_at
	`, planID, utcHour, intervalHours, cleanupLocalDays, cleanupRemoteDays, format, now, now)
	if err != nil {
		return fmt.Errorf("set snapshot plan: %w", err)
	}
	return nil
}

// GetPlan returns the schedule, or (nil, nil) when none is configured.
//
// "Not configured" is an ordinary state, not an error: most deployments have no
// snapshot schedule, and the scheduler asks on every tick.
func (s *SnapshotStore) GetPlan() (*Plan, error) {
	var plan Plan
	err := s.db.Get(&plan, `SELECT * FROM snapshot_plans WHERE id = ?`, planID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get snapshot plan: %w", err)
	}
	return &plan, nil
}

func (s *SnapshotStore) DeletePlan() error {
	_, err := s.db.Exec(`DELETE FROM snapshot_plans WHERE id = ?`, planID)
	if err != nil {
		return fmt.Errorf("delete snapshot plan: %w", err)
	}
	return nil
}

// RecordSnapshot inserts a completed snapshot into the catalogue and fills in
// the record's ID.
//
// Deliberately called AFTER the archive is built, never before. MakeSnapshot
// copies oddk.db into the archive partway through, so a record inserted first
// would be captured mid-flight and every restore of that archive would carry a
// permanently unfinished row describing itself. See internal-docs/snapshots.done.md.
func (s *SnapshotStore) RecordSnapshot(rec *Record) error {
	local := rec.LocalLocation
	if !local.Valid && rec.LocalPath != "" {
		local = sql.NullString{String: rec.LocalPath, Valid: true}
	}
	comment := rec.Comment
	if !comment.Valid && rec.CommentStr != "" {
		comment = sql.NullString{String: rec.CommentStr, Valid: true}
	}

	format := rec.Format
	if format == "" {
		format = "logical"
	}

	// Persist the per-instance list when the caller provided one. A nil slice
	// stores NULL (unknown), matching pre-019 rows; an empty non-nil slice
	// stores "[]" — a snapshot of a deployment with no instances.
	instancesJSON := rec.InstancesJSON
	if !instancesJSON.Valid && rec.Instances != nil {
		encoded, err := json.Marshal(rec.Instances)
		if err != nil {
			return fmt.Errorf("encode snapshot instances: %w", err)
		}
		instancesJSON = sql.NullString{String: string(encoded), Valid: true}
	}

	result, err := s.db.Exec(`
		INSERT INTO snapshot_history
			(filename, created_at, size, status, instances_with_data, config_only, format, instances_json, local_location, remote_location, comment)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, rec.Filename, rec.CreatedAt, rec.Size, rec.Status,
		rec.InstancesWithData, rec.ConfigOnly, format, instancesJSON, local, rec.RemoteLocation, comment)
	if err != nil {
		return fmt.Errorf("record snapshot: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("read snapshot record id: %w", err)
	}
	rec.ID = int(id)
	rec.LocalLocation = local
	rec.Comment = comment
	return nil
}

func (s *SnapshotStore) List() ([]*Record, error) {
	var records []*Record
	if err := s.db.Select(&records, `SELECT * FROM snapshot_history ORDER BY created_at DESC`); err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	for _, r := range records {
		flatten(r)
	}
	return records, nil
}

func (s *SnapshotStore) Get(id int) (*Record, error) {
	var rec Record
	err := s.db.Get(&rec, `SELECT * FROM snapshot_history WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get snapshot %d: %w", id, err)
	}
	flatten(&rec)
	return &rec, nil
}

// SetRemoteLocation records an offsite copy.
func (s *SnapshotStore) SetRemoteLocation(id int, remote string) error {
	_, err := s.db.Exec(`UPDATE snapshot_history SET remote_location = ? WHERE id = ?`, remote, id)
	if err != nil {
		return fmt.Errorf("set snapshot remote location: %w", err)
	}
	return nil
}

// ClearLocalLocation drops the local copy's path, for use after the file has
// been deleted by retention.
//
// It refuses when there is no remote copy rather than letting the schema CHECK
// fail mid-operation: a row with neither location describes nothing, so the
// caller must delete the record instead. Learning this the hard way in the
// backup catalogue is why it is stated here.
func (s *SnapshotStore) ClearLocalLocation(id int) error {
	var remote sql.NullString
	if err := s.db.Get(&remote, `SELECT remote_location FROM snapshot_history WHERE id = ?`, id); err != nil {
		return fmt.Errorf("read snapshot %d before clearing local location: %w", id, err)
	}
	if !remote.Valid || remote.String == "" {
		return fmt.Errorf("snapshot %d has no remote copy, so clearing its local location would leave a record describing nothing; delete the record instead", id)
	}
	if _, err := s.db.Exec(`UPDATE snapshot_history SET local_location = NULL WHERE id = ?`, id); err != nil {
		return fmt.Errorf("clear snapshot local location: %w", err)
	}
	return nil
}

func (s *SnapshotStore) ClearRemoteLocation(id int) error {
	var local sql.NullString
	if err := s.db.Get(&local, `SELECT local_location FROM snapshot_history WHERE id = ?`, id); err != nil {
		return fmt.Errorf("read snapshot %d before clearing remote location: %w", id, err)
	}
	if !local.Valid || local.String == "" {
		return fmt.Errorf("snapshot %d has no local copy, so clearing its remote location would leave a record describing nothing; delete the record instead", id)
	}
	if _, err := s.db.Exec(`UPDATE snapshot_history SET remote_location = NULL WHERE id = ?`, id); err != nil {
		return fmt.Errorf("clear snapshot remote location: %w", err)
	}
	return nil
}

func (s *SnapshotStore) Delete(id int) error {
	_, err := s.db.Exec(`DELETE FROM snapshot_history WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete snapshot record: %w", err)
	}
	return nil
}

// ReconcileLocalLocations re-points snapshot records at this host's backup
// directory, mirroring BackupStore.ReconcileLocalLocations.
//
// Required after `snapshot apply`: the restored oddk.db carries the SOURCE
// host's absolute paths, and the snapshot archive does not contain other
// snapshots. Without this the catalogue claims local copies that are not here —
// `snapshot list` lies about where a snapshot exists, and retention operates on
// paths that were never on this machine.
//
// A record whose file is absent and which has no remote copy is left alone and
// COUNTED, not deleted: the schema CHECK forbids a row with neither location, so
// clearing it would fail the constraint mid-apply.
func (s *SnapshotStore) ReconcileLocalLocations(backupDir string) (repointed, cleared, danglingAfter int, err error) {
	type row struct {
		ID       int            `db:"id"`
		LocalLoc sql.NullString `db:"local_location"`
		Remote   sql.NullString `db:"remote_location"`
	}
	var rows []row
	if err := s.db.Select(&rows, `SELECT id, local_location, remote_location FROM snapshot_history`); err != nil {
		return 0, 0, 0, fmt.Errorf("read snapshot history: %w", err)
	}

	for _, r := range rows {
		hasRemote := r.Remote.Valid && r.Remote.String != ""
		if !r.LocalLoc.Valid || r.LocalLoc.String == "" {
			if !hasRemote {
				danglingAfter++
			}
			continue
		}

		candidate := filepath.Join(backupDir, filepath.Base(r.LocalLoc.String))
		if _, statErr := os.Stat(candidate); statErr == nil {
			if candidate != r.LocalLoc.String {
				if _, execErr := s.db.Exec(
					`UPDATE snapshot_history SET local_location = ? WHERE id = ?`, candidate, r.ID); execErr != nil {
					return repointed, cleared, danglingAfter, fmt.Errorf("re-point snapshot %d: %w", r.ID, execErr)
				}
				repointed++
			}
			continue
		}

		if !hasRemote {
			danglingAfter++
			continue
		}
		if _, execErr := s.db.Exec(
			`UPDATE snapshot_history SET local_location = NULL WHERE id = ?`, r.ID); execErr != nil {
			return repointed, cleared, danglingAfter, fmt.Errorf("clear local location of snapshot %d: %w", r.ID, execErr)
		}
		cleared++
	}
	return repointed, cleared, danglingAfter, nil
}

// ReferencedFilenames returns the base filenames of every snapshot the catalogue
// records a local copy for, so the startup sweep can tell a managed archive from
// a genuine orphan.
func (s *SnapshotStore) ReferencedFilenames() (map[string]bool, error) {
	var locations []sql.NullString
	if err := s.db.Select(&locations,
		`SELECT local_location FROM snapshot_history WHERE local_location IS NOT NULL`); err != nil {
		return nil, fmt.Errorf("read snapshot locations: %w", err)
	}
	referenced := make(map[string]bool, len(locations))
	for _, loc := range locations {
		if loc.Valid && loc.String != "" {
			referenced[filepath.Base(loc.String)] = true
		}
	}
	return referenced, nil
}

func flatten(r *Record) {
	if r.LocalLocation.Valid {
		r.LocalPath = r.LocalLocation.String
	}
	if r.RemoteLocation.Valid {
		r.RemotePath = r.RemoteLocation.String
	}
	if r.Comment.Valid {
		r.CommentStr = r.Comment.String
	}
	if r.InstancesJSON.Valid && r.InstancesJSON.String != "" {
		var instances []RecordInstance
		if err := json.Unmarshal([]byte(r.InstancesJSON.String), &instances); err != nil {
			// A malformed column must read as "unknown" (like a pre-019 row),
			// not crash a listing; the record itself is still usable.
			log.Printf("WARNING: snapshot %d has malformed instances_json (%v); treating coverage as unknown", r.ID, err)
		} else if instances == nil {
			r.Instances = []RecordInstance{}
		} else {
			r.Instances = instances
		}
	}
}

// SetLocalLocation records a local copy, for use after a download.
func (s *SnapshotStore) SetLocalLocation(id int, local string) error {
	if _, err := s.db.Exec(`UPDATE snapshot_history SET local_location = ? WHERE id = ?`, local, id); err != nil {
		return fmt.Errorf("set snapshot local location: %w", err)
	}
	return nil
}
