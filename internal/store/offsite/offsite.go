package offsite

import (
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/hypersequent/oddk/internal/rfc3339time"
)

type OffsiteStore struct {
	db *sqlx.DB
}

func NewOffsiteStore(db *sqlx.DB) *OffsiteStore {
	return &OffsiteStore{db: db}
}

func (s *OffsiteStore) GetActive() (*OffsiteSettings, error) {
	var settings OffsiteSettings
	err := s.db.QueryRow(`
		SELECT id, active, type, bucket, endpoint, region, access_key_id,
		       secret_access_key, bucket_path, ec2_iam_role, created_at, updated_at
		FROM offsite_settings
		WHERE active = 1
		ORDER BY id DESC
		LIMIT 1
	`).Scan(&settings.ID, &settings.Active, &settings.Type, &settings.Bucket,
		&settings.Endpoint, &settings.Region, &settings.AccessKeyID,
		&settings.SecretAccessKey, &settings.BucketPath, &settings.EC2IAMRole, &settings.CreatedAt, &settings.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get active offsite settings: %w", err)
	}

	return &settings, nil
}

func (s *OffsiteStore) GetByID(id int64) (*OffsiteSettings, error) {
	var settings OffsiteSettings
	err := s.db.QueryRow(`
		SELECT id, active, type, bucket, endpoint, region, access_key_id,
		       secret_access_key, bucket_path, ec2_iam_role, created_at, updated_at
		FROM offsite_settings
		WHERE id = ?
	`, id).Scan(&settings.ID, &settings.Active, &settings.Type, &settings.Bucket,
		&settings.Endpoint, &settings.Region, &settings.AccessKeyID,
		&settings.SecretAccessKey, &settings.BucketPath, &settings.EC2IAMRole, &settings.CreatedAt, &settings.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("offsite settings with id %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get offsite settings by id: %w", err)
	}

	return &settings, nil
}

func (s *OffsiteStore) Create(settings *OffsiteSettings) error {
	tx, err := s.db.Beginx()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Deactivate any existing active settings
	_, err = tx.Exec(`UPDATE offsite_settings SET active = 0, secret_access_key = 'REDACTED' WHERE active = 1`)
	if err != nil {
		return fmt.Errorf("deactivate existing settings: %w", err)
	}

	now := rfc3339time.Now()
	result, err := tx.Exec(`
		INSERT INTO offsite_settings (active, type, bucket, endpoint, region, access_key_id,
		                              secret_access_key, bucket_path, ec2_iam_role, created_at, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, settings.Type, settings.Bucket, settings.Endpoint, settings.Region,
		settings.AccessKeyID, settings.SecretAccessKey, settings.BucketPath, settings.EC2IAMRole, now, now)
	if err != nil {
		return fmt.Errorf("insert offsite settings: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get last insert id: %w", err)
	}
	settings.ID = id
	settings.Active = true
	settings.CreatedAt = now
	settings.UpdatedAt = now

	return tx.Commit()
}

func (s *OffsiteStore) Remove() error {
	tx, err := s.db.Beginx()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var activeID int64
	err = tx.Get(&activeID, `SELECT id FROM offsite_settings WHERE active = 1`)
	if err == sql.ErrNoRows {
		return fmt.Errorf("no active offsite configuration found")
	}
	if err != nil {
		return fmt.Errorf("get active settings: %w", err)
	}

	// Check if there are any logs for this configuration
	var logCount int
	err = tx.Get(&logCount, `SELECT COUNT(*) FROM offsite_logs WHERE offsite_settings_id = ?`, activeID)
	if err != nil {
		return fmt.Errorf("count logs: %w", err)
	}

	if logCount == 0 {
		// No logs, safe to delete
		_, err = tx.Exec(`DELETE FROM offsite_settings WHERE id = ?`, activeID)
		if err != nil {
			return fmt.Errorf("delete offsite settings: %w", err)
		}
	} else {
		// Has logs, just deactivate and redact secret
		_, err = tx.Exec(`UPDATE offsite_settings SET active = 0, secret_access_key = 'REDACTED' WHERE id = ?`, activeID)
		if err != nil {
			return fmt.Errorf("deactivate offsite settings: %w", err)
		}
	}

	return tx.Commit()
}

func (s *OffsiteStore) AddLog(log *OffsiteLog) error {
	now := rfc3339time.Now()
	result, err := s.db.Exec(`
		INSERT INTO offsite_logs (event, offsite_settings_id, object, success, error_details, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, log.Event, log.OffsiteSettingsID, log.Object, log.Success, log.ErrorDetails, now)
	if err != nil {
		return fmt.Errorf("insert offsite log: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get last insert id: %w", err)
	}
	log.ID = id
	log.CreatedAt = now

	return nil
}

func (s *OffsiteStore) GetLogs(limit int) ([]OffsiteLog, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := s.db.Query(`
		SELECT id, event, offsite_settings_id, object, success, error_details, created_at
		FROM offsite_logs
		ORDER BY created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get offsite logs: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var logs []OffsiteLog
	for rows.Next() {
		var log OffsiteLog
		err := rows.Scan(&log.ID, &log.Event, &log.OffsiteSettingsID, &log.Object,
			&log.Success, &log.ErrorDetails, &log.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scan log row: %w", err)
		}
		logs = append(logs, log)
	}

	return logs, nil
}

// SecretRow pairs an offsite settings row ID with its stored (encrypted)
// secret access key, for the daemon's startup re-encryption sweep.
type SecretRow struct {
	ID              int64  `db:"id"`
	SecretAccessKey string `db:"secret_access_key"`
}

// ListAllSecrets returns the stored secret of every offsite settings row
// (active or not — inactive rows back the '%SAME-AS-BEFORE%' apply flow).
func (s *OffsiteStore) ListAllSecrets() ([]SecretRow, error) {
	var rows []SecretRow
	if err := s.db.Select(&rows, `SELECT id, secret_access_key FROM offsite_settings`); err != nil {
		return nil, fmt.Errorf("list offsite secrets: %w", err)
	}
	return rows, nil
}

// UpdateSecretAccessKey replaces the stored secret of one settings row.
func (s *OffsiteStore) UpdateSecretAccessKey(id int64, encryptedSecret string) error {
	result, err := s.db.Exec(`UPDATE offsite_settings SET secret_access_key = ? WHERE id = ?`, encryptedSecret, id)
	if err != nil {
		return fmt.Errorf("update offsite secret: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update offsite secret: rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("offsite settings row %d not found", id)
	}
	return nil
}

func (s *OffsiteStore) GetPreviousSecretKey() (string, error) {
	var secretKey string
	err := s.db.Get(&secretKey, `
		SELECT secret_access_key
		FROM offsite_settings
		WHERE active = 1 AND secret_access_key != 'REDACTED'
		ORDER BY id DESC
		LIMIT 1
	`)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no active configuration with valid secret key found")
	}
	if err != nil {
		return "", fmt.Errorf("get previous secret key: %w", err)
	}
	return secretKey, nil
}
