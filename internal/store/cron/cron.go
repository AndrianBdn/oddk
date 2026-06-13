package cron

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/hypersequent/oddk/internal/rfc3339time"
)

type CronStore struct {
	db *sqlx.DB
}

func NewCronStore(db *sqlx.DB) *CronStore {
	return &CronStore{db: db}
}

func (s *CronStore) CreatePlan(instanceName string, utcHour, cleanupLocalDays, cleanupRemoteDays int) error {
	now := rfc3339time.Now()

	// Use INSERT ON CONFLICT to preserve created_at on update
	_, err := s.db.Exec(`
		INSERT INTO cron_plans (instance_name, utc_hour, cleanup_local_days, cleanup_remote_days, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(instance_name) DO UPDATE SET
			utc_hour = excluded.utc_hour,
			cleanup_local_days = excluded.cleanup_local_days,
			cleanup_remote_days = excluded.cleanup_remote_days,
			updated_at = excluded.updated_at
	`, instanceName, utcHour, cleanupLocalDays, cleanupRemoteDays, now, now)
	if err != nil {
		return fmt.Errorf("create or update cron plan: %w", err)
	}
	return nil
}

func (s *CronStore) DeletePlan(instanceName string) error {
	_, err := s.db.Exec(`DELETE FROM cron_plans WHERE instance_name = ?`, instanceName)
	return err
}

func (s *CronStore) GetPlan(instanceName string) (*CronPlan, error) {
	var plan CronPlan
	err := s.db.Get(&plan, `SELECT * FROM cron_plans WHERE instance_name = ?`, instanceName)
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

func (s *CronStore) ListPlans() ([]*CronPlan, error) {
	var plans []*CronPlan
	err := s.db.Select(&plans, `SELECT * FROM cron_plans ORDER BY instance_name`)
	return plans, err
}

func (s *CronStore) GetPlansForHour(utcHour int) ([]*CronPlan, error) {
	var plans []*CronPlan
	err := s.db.Select(&plans, `SELECT * FROM cron_plans WHERE utc_hour = ?`, utcHour)
	return plans, err
}

func (s *CronStore) GetAllPlans() ([]*CronPlan, error) {
	var plans []*CronPlan
	err := s.db.Select(&plans, `SELECT * FROM cron_plans ORDER BY instance_name`)
	return plans, err
}

func (s *CronStore) CreateLog(instanceName string) (*CronLog, error) {
	startedAt := rfc3339time.Now()
	result, err := s.db.Exec(`
		INSERT INTO cron_logs (
			instance_name,
			started_at
		) VALUES (?, ?)
	`, instanceName, startedAt)
	if err != nil {
		return nil, fmt.Errorf("create cron log: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get cron log id: %w", err)
	}

	return &CronLog{
		ID:           int(id),
		InstanceName: instanceName,
		StartedAt:    startedAt,
	}, nil
}

func (s *CronStore) UpdateLog(logID int, updates map[string]any) error {
	setClause := ""
	values := []any{}

	for key, value := range updates {
		if setClause != "" {
			setClause += ", "
		}
		setClause += fmt.Sprintf("%s = ?", key)
		values = append(values, value)
	}

	if setClause == "" {
		return nil // Nothing to update
	}

	values = append(values, logID)
	query := fmt.Sprintf("UPDATE cron_logs SET %s WHERE id = ?", setClause)

	_, err := s.db.Exec(query, values...)
	if err != nil {
		return fmt.Errorf("update cron log: %w", err)
	}

	return nil
}

func (s *CronStore) CompleteLog(logID int) error {
	now := rfc3339time.Now()
	return s.UpdateLog(logID, map[string]any{
		"completed_at": now,
	})
}

func (s *CronStore) ListLogs(instanceName string, limit int) ([]*CronLog, error) {
	var logs []*CronLog
	query := `SELECT * FROM cron_logs WHERE instance_name = ? ORDER BY started_at DESC`
	args := []any{instanceName}

	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	err := s.db.Select(&logs, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list cron logs: %w", err)
	}

	return logs, nil
}

func (s *CronStore) GetLatestLogForInstance(instanceName string) (*CronLog, error) {
	var log CronLog
	err := s.db.Get(&log, `
		SELECT * FROM cron_logs 
		WHERE instance_name = ? 
		ORDER BY started_at DESC 
		LIMIT 1
	`, instanceName)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest cron log: %w", err)
	}

	return &log, nil
}

func (s *CronStore) HasRunInLastHour(instanceName string) (bool, error) {
	var count int
	oneHourAgo := rfc3339time.Time{Time: rfc3339time.Now().Add(-time.Hour)}
	err := s.db.Get(&count, `
		SELECT COUNT(*) FROM cron_logs 
		WHERE instance_name = ? AND started_at > ?
	`, instanceName, oneHourAgo)
	if err != nil {
		return false, fmt.Errorf("check recent cron runs: %w", err)
	}

	return count > 0, nil
}

func (s *CronStore) CheckPlanExists(instanceName string) (bool, error) {
	var count int
	err := s.db.Get(&count, `SELECT COUNT(*) FROM cron_plans WHERE instance_name = ?`, instanceName)
	if err != nil {
		return false, fmt.Errorf("check cron plan exists: %w", err)
	}
	return count > 0, nil
}
