package backup

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/jmoiron/sqlx"

	"github.com/hypersequent/oddk/internal/rfc3339time"
)

type BackupStore struct {
	db      *sqlx.DB
	dataDir string // For filesystem validation
}

func NewBackupStore(db *sqlx.DB, dataDir string) *BackupStore {
	return &BackupStore{
		db:      db,
		dataDir: dataDir,
	}
}

func (s *BackupStore) RecordBackup(record *BackupRecord) error {
	now := rfc3339time.Now()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}

	// For new backups, LocalPath should be set
	if record.LocalPath != "" {
		record.LocalLocation = sql.NullString{String: record.LocalPath, Valid: true}
	}

	result, err := s.db.Exec(`
		INSERT INTO backup_history (instance_name, timestamp, size, local_location, remote_location, status, comment, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, record.InstanceName, record.Timestamp, record.Size, record.LocalLocation, record.RemoteLocation, record.Status, record.Comment, record.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert backup record: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get backup ID: %w", err)
	}
	record.ID = int(id)

	return nil
}

func (s *BackupStore) ListAllBackups() ([]BackupRecord, error) {
	var backups []BackupRecord
	query := `SELECT * FROM backup_history ORDER BY timestamp DESC`
	err := s.db.Select(&backups, query)
	if err != nil {
		return nil, fmt.Errorf("list all backups: %w", err)
	}

	var validBackups []BackupRecord
	var orphaned []int

	for i := range backups {
		// Convert Comment from sql.NullString to regular string
		if backups[i].Comment.Valid {
			backups[i].CommentStr = backups[i].Comment.String
		}
		// Convert locations for JSON output
		if backups[i].LocalLocation.Valid {
			backups[i].LocalPath = backups[i].LocalLocation.String
		}
		if backups[i].RemoteLocation.Valid {
			backups[i].RemotePath = backups[i].RemoteLocation.String
		}

		if backups[i].LocalLocation.Valid {
			s.validateBackupFile(&backups[i])
		}

		// Collect orphaned records for cleanup (no local or remote copy)
		switch {
		case backups[i].Status == "completed" &&
			!backups[i].LocalLocation.Valid &&
			!backups[i].RemoteLocation.Valid:
			orphaned = append(orphaned, backups[i].ID)
			log.Printf("Warning: found orphaned backup record with no locations - instance=%s backup_id=%d",
				backups[i].InstanceName, backups[i].ID)
		case backups[i].LocalLocation.Valid && !backups[i].FileExists && !backups[i].RemoteLocation.Valid:
			// Local file missing and no remote backup
			orphaned = append(orphaned, backups[i].ID)
			log.Printf("Warning: found orphaned backup record - instance=%s backup_id=%d local_location=%s",
				backups[i].InstanceName, backups[i].ID, backups[i].LocalLocation.String)
		default:
			if backups[i].FileExists && backups[i].ActualSize != backups[i].Size {
				log.Printf("Warning: backup size mismatch - instance=%s backup_id=%d recorded=%d actual=%d",
					backups[i].InstanceName, backups[i].ID, backups[i].Size, backups[i].ActualSize)
				if err := s.updateBackupSize(backups[i].ID, backups[i].ActualSize); err != nil {
					log.Printf("Error: failed to update backup size - backup_id=%d error=%v", backups[i].ID, err)
				} else {
					backups[i].Size = backups[i].ActualSize
				}
			}
			validBackups = append(validBackups, backups[i])
		}
	}

	if len(orphaned) > 0 {
		if err := s.cleanupOrphanedRecords(orphaned); err != nil {
			log.Printf("Error: failed to cleanup orphaned records: %v", err)
		}
	}

	return validBackups, nil
}

func (s *BackupStore) ListBackups(instanceName string) ([]BackupRecord, error) {
	var backups []BackupRecord
	query := `SELECT * FROM backup_history WHERE instance_name = ? ORDER BY timestamp DESC`
	err := s.db.Select(&backups, query, instanceName)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}

	var validBackups []BackupRecord
	var orphaned []int

	for i := range backups {
		// Convert Comment from sql.NullString to regular string
		if backups[i].Comment.Valid {
			backups[i].CommentStr = backups[i].Comment.String
		}
		// Convert locations for JSON output
		if backups[i].LocalLocation.Valid {
			backups[i].LocalPath = backups[i].LocalLocation.String
		}
		if backups[i].RemoteLocation.Valid {
			backups[i].RemotePath = backups[i].RemoteLocation.String
		}

		if backups[i].LocalLocation.Valid {
			s.validateBackupFile(&backups[i])
		}

		// Collect orphaned records for cleanup (no local or remote copy)
		switch {
		case backups[i].Status == "completed" &&
			!backups[i].LocalLocation.Valid &&
			!backups[i].RemoteLocation.Valid:
			orphaned = append(orphaned, backups[i].ID)
			log.Printf("Warning: found orphaned backup record with no locations - instance=%s backup_id=%d",
				instanceName, backups[i].ID)
		case backups[i].LocalLocation.Valid && !backups[i].FileExists && !backups[i].RemoteLocation.Valid:
			// Local file missing and no remote backup
			orphaned = append(orphaned, backups[i].ID)
			log.Printf("Warning: found orphaned backup record - instance=%s backup_id=%d local_location=%s",
				instanceName, backups[i].ID, backups[i].LocalLocation.String)
		default:
			if backups[i].FileExists && backups[i].ActualSize != backups[i].Size {
				log.Printf("Warning: backup size mismatch - instance=%s backup_id=%d recorded=%d actual=%d",
					instanceName, backups[i].ID, backups[i].Size, backups[i].ActualSize)
				if err := s.updateBackupSize(backups[i].ID, backups[i].ActualSize); err != nil {
					log.Printf("Error: failed to update backup size - backup_id=%d error=%v", backups[i].ID, err)
				} else {
					backups[i].Size = backups[i].ActualSize
				}
			}
			validBackups = append(validBackups, backups[i])
		}
	}

	if len(orphaned) > 0 {
		if err := s.cleanupOrphanedRecords(orphaned); err != nil {
			log.Printf("Error: failed to cleanup orphaned records: %v", err)
		}
	}

	return validBackups, nil
}

func (s *BackupStore) GetBackupByID(backupID int) (*BackupRecord, error) {
	var backup BackupRecord
	query := `SELECT * FROM backup_history WHERE id = ?`
	err := s.db.Get(&backup, query, backupID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("backup not found: %d", backupID)
		}
		return nil, fmt.Errorf("get backup: %w", err)
	}

	// Convert for output
	if backup.Comment.Valid {
		backup.CommentStr = backup.Comment.String
	}
	if backup.LocalLocation.Valid {
		backup.LocalPath = backup.LocalLocation.String
		s.validateBackupFile(&backup)
	}
	if backup.RemoteLocation.Valid {
		backup.RemotePath = backup.RemoteLocation.String
	}

	return &backup, nil
}

func (s *BackupStore) UpdateRemoteLocation(backupID int, remoteLocation string) error {
	_, err := s.db.Exec(`UPDATE backup_history SET remote_location = ? WHERE id = ?`,
		sql.NullString{String: remoteLocation, Valid: true}, backupID)
	if err != nil {
		return fmt.Errorf("update remote location: %w", err)
	}
	return nil
}

func (s *BackupStore) UpdateLocalLocation(backupID int, localLocation string) error {
	_, err := s.db.Exec(`UPDATE backup_history SET local_location = ? WHERE id = ?`,
		sql.NullString{String: localLocation, Valid: true}, backupID)
	if err != nil {
		return fmt.Errorf("update local location: %w", err)
	}
	return nil
}

func (s *BackupStore) ClearLocalLocation(backupID int) error {
	_, err := s.db.Exec(`UPDATE backup_history SET local_location = NULL WHERE id = ?`, backupID)
	if err != nil {
		return fmt.Errorf("clear local location: %w", err)
	}
	return nil
}

func (s *BackupStore) RemoveLocalCopy(backupID int, instanceName string) error {
	// First, get the backup to verify it exists and belongs to the instance
	backup, err := s.GetBackupByID(backupID)
	if err != nil {
		return err
	}

	if backup.InstanceName != instanceName {
		return fmt.Errorf("backup %d does not belong to instance %s", backupID, instanceName)
	}

	if !backup.LocalLocation.Valid {
		return fmt.Errorf("backup %d has no local copy", backupID)
	}

	backupPath := backup.LocalLocation.String
	if !filepath.IsAbs(backupPath) {
		backupPath = filepath.Join(s.dataDir, backupPath)
	}

	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete local backup file: %w", err)
	}

	// If no remote copy exists, delete the entire record
	// Otherwise, clear the local location in database
	if !backup.RemoteLocation.Valid {
		_, err = s.db.Exec(`DELETE FROM backup_history WHERE id = ?`, backupID)
		if err != nil {
			return fmt.Errorf("delete backup record: %w", err)
		}
	} else {
		// Only clear location if remote exists (to maintain constraint)
		if err := s.ClearLocalLocation(backupID); err != nil {
			return err
		}
	}

	return nil
}

func (s *BackupStore) RemoveRemoteCopy(backupID int, instanceName string) error {
	// First, get the backup to verify it exists and belongs to the instance
	backup, err := s.GetBackupByID(backupID)
	if err != nil {
		return err
	}

	if backup.InstanceName != instanceName {
		return fmt.Errorf("backup %d does not belong to instance %s", backupID, instanceName)
	}

	if !backup.RemoteLocation.Valid {
		return fmt.Errorf("backup %d has no remote copy", backupID)
	}

	// If no local copy exists, delete the entire record
	// Otherwise, clear the remote location in database
	if !backup.LocalLocation.Valid {
		_, err = s.db.Exec(`DELETE FROM backup_history WHERE id = ?`, backupID)
		if err != nil {
			return fmt.Errorf("delete backup record: %w", err)
		}
	} else {
		// Only clear location if local exists (to maintain constraint)
		_, err = s.db.Exec(`UPDATE backup_history SET remote_location = NULL WHERE id = ?`, backupID)
		if err != nil {
			return fmt.Errorf("clear remote location: %w", err)
		}
	}

	return nil
}

func (s *BackupStore) validateBackupFile(backup *BackupRecord) {
	if !backup.LocalLocation.Valid {
		backup.FileExists = false
		return
	}

	backupPath := backup.LocalLocation.String

	// Check if path is relative or absolute
	if !filepath.IsAbs(backupPath) {
		// Relative path - prepend data directory
		backupPath = filepath.Join(s.dataDir, backupPath)
	}

	info, err := os.Stat(backupPath)
	if err != nil {
		if os.IsNotExist(err) {
			backup.FileExists = false
		} else {
			log.Printf("Error: failed to stat backup file %s: %v", backupPath, err)
			backup.FileExists = false
		}
		return
	}

	backup.FileExists = true
	backup.ActualSize = info.Size()
}

func (s *BackupStore) updateBackupSize(backupID int, actualSize int64) error {
	_, err := s.db.Exec(`UPDATE backup_history SET size = ? WHERE id = ?`, actualSize, backupID)
	if err != nil {
		return fmt.Errorf("update backup size: %w", err)
	}
	return nil
}

func (s *BackupStore) cleanupOrphanedRecord(backupID int) error {
	_, err := s.db.Exec(`DELETE FROM backup_history WHERE id = ?`, backupID)
	if err != nil {
		return fmt.Errorf("delete orphaned backup: %w", err)
	}
	return nil
}

func (s *BackupStore) cleanupOrphanedRecords(orphaned []int) error {
	for _, id := range orphaned {
		if err := s.cleanupOrphanedRecord(id); err != nil {
			return err
		}
		log.Printf("Info: cleaned up orphaned backup record - backup_id=%d", id)
	}
	return nil
}
