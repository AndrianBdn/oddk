package health

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

type HealthStore struct {
	db *sqlx.DB
}

func NewHealthStore(db *sqlx.DB) *HealthStore {
	return &HealthStore{db: db}
}

// StartHealthCheck creates a new health record with in_progress=true
func (s *HealthStore) StartHealthCheck() (*HealthRecord, error) {
	record := &HealthRecord{
		TsUnix:           time.Now().Unix(),
		InProgress:       true,
		HealthyAll:       true, // optimistic
		HealthyHost:      true, // optimistic
		HealthyInstances: "",
		BrokenInstances:  "",
		FailDetails:      "",
	}

	result, err := s.db.Exec(`INSERT INTO health (ts_unix, in_progress, healthy_all, healthy_host, healthy_instances, broken_instances, fail_details)
							   VALUES (?, ?, ?, ?, ?, ?, ?)`,
		record.TsUnix,
		boolToInt(record.InProgress),
		boolToInt(record.HealthyAll),
		boolToInt(record.HealthyHost),
		record.HealthyInstances,
		record.BrokenInstances,
		record.FailDetails,
	)
	if err != nil {
		return nil, fmt.Errorf("insert health record: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get health record id: %w", err)
	}

	record.ID = id

	return record, nil
}

// UpdateHealthCheck updates the health record with final results
func (s *HealthStore) UpdateHealthCheck(id int64, healthyHost bool, healthyInstances, brokenInstances []string, failDetails string) error {
	healthyAll := healthyHost && len(brokenInstances) == 0

	query := `UPDATE health 
			  SET in_progress = 0, 
				  healthy_all = ?, 
				  healthy_host = ?, 
				  healthy_instances = ?, 
				  broken_instances = ?, 
				  fail_details = ?
			  WHERE id = ?`

	_, err := s.db.Exec(query,
		boolToInt(healthyAll),
		boolToInt(healthyHost),
		strings.Join(healthyInstances, ","),
		strings.Join(brokenInstances, ","),
		failDetails,
		id,
	)
	if err != nil {
		return fmt.Errorf("update health record: %w", err)
	}

	return nil
}

// GetLatestHealthRecord returns the most recent health record
func (s *HealthStore) GetLatestHealthRecord() (*HealthRecord, error) {
	query := `SELECT id, ts_unix, in_progress, healthy_all, healthy_host, 
					 healthy_instances, broken_instances, fail_details
			  FROM health 
			  ORDER BY ts_unix DESC 
			  LIMIT 1`

	row := s.db.QueryRow(query)

	var record HealthRecord
	var inProgress, healthyAll, healthyHost int

	err := row.Scan(
		&record.ID,
		&record.TsUnix,
		&inProgress,
		&healthyAll,
		&healthyHost,
		&record.HealthyInstances,
		&record.BrokenInstances,
		&record.FailDetails,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // No records yet
		}
		return nil, fmt.Errorf("get latest health record: %w", err)
	}

	record.InProgress = intToBool(inProgress)
	record.HealthyAll = intToBool(healthyAll)
	record.HealthyHost = intToBool(healthyHost)

	return &record, nil
}

// GetHealthHistory returns recent health records
func (s *HealthStore) GetHealthHistory(limit int) ([]*HealthRecord, error) {
	query := `SELECT id, ts_unix, in_progress, healthy_all, healthy_host, 
					 healthy_instances, broken_instances, fail_details
			  FROM health 
			  ORDER BY ts_unix DESC 
			  LIMIT ?`

	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("get health history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []*HealthRecord
	for rows.Next() {
		var record HealthRecord
		var inProgress, healthyAll, healthyHost int

		err := rows.Scan(
			&record.ID,
			&record.TsUnix,
			&inProgress,
			&healthyAll,
			&healthyHost,
			&record.HealthyInstances,
			&record.BrokenInstances,
			&record.FailDetails,
		)
		if err != nil {
			return nil, fmt.Errorf("scan health record: %w", err)
		}

		record.InProgress = intToBool(inProgress)
		record.HealthyAll = intToBool(healthyAll)
		record.HealthyHost = intToBool(healthyHost)

		records = append(records, &record)
	}

	return records, nil
}

// CleanupOldRecords removes health records older than the specified duration
func (s *HealthStore) CleanupOldRecords(olderThan time.Duration) error {
	cutoff := time.Now().Add(-olderThan).Unix()
	query := `DELETE FROM health WHERE ts_unix < ?`

	result, err := s.db.Exec(query, cutoff)
	if err != nil {
		return fmt.Errorf("cleanup old health records: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected > 0 {
		fmt.Printf("Cleaned up %d old health records\n", rowsAffected)
	}

	return nil
}

// ResetInProgressRecords resets any stuck in_progress records on startup
func (s *HealthStore) ResetInProgressRecords() error {
	query := `UPDATE health SET in_progress = 0 WHERE in_progress = 1`

	result, err := s.db.Exec(query)
	if err != nil {
		return fmt.Errorf("reset in_progress records: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected > 0 {
		fmt.Printf("Reset %d stuck in_progress health records\n", rowsAffected)
	}

	return nil
}

// GetRecentHealthRecords returns the most recent N completed health records (excluding in_progress)
// Used for notification threshold evaluation
func (s *HealthStore) GetRecentHealthRecords(limit int) ([]*HealthRecord, error) {
	query := `SELECT id, ts_unix, in_progress, healthy_all, healthy_host, 
					 healthy_instances, broken_instances, fail_details
			  FROM health 
			  WHERE in_progress = 0
			  ORDER BY ts_unix DESC 
			  LIMIT ?`

	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("get recent health records: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []*HealthRecord
	for rows.Next() {
		var record HealthRecord
		var inProgress, healthyAll, healthyHost int

		err := rows.Scan(
			&record.ID,
			&record.TsUnix,
			&inProgress,
			&healthyAll,
			&healthyHost,
			&record.HealthyInstances,
			&record.BrokenInstances,
			&record.FailDetails,
		)
		if err != nil {
			return nil, fmt.Errorf("scan health record: %w", err)
		}

		record.InProgress = intToBool(inProgress)
		record.HealthyAll = intToBool(healthyAll)
		record.HealthyHost = intToBool(healthyHost)

		records = append(records, &record)
	}

	return records, nil
}

// Helper functions
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func intToBool(i int) bool {
	return i != 0
}
