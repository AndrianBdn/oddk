package notifications

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/hypersequent/oddk/internal/rfc3339time"
)

type NotificationStore struct {
	db *sqlx.DB
}

func NewNotificationStore(db *sqlx.DB) *NotificationStore {
	return &NotificationStore{db: db}
}

func (s *NotificationStore) Create(name string, notifType NotificationType, config json.RawMessage) (*Notification, error) {
	if err := ValidateNotificationType(string(notifType)); err != nil {
		return nil, err
	}
	if err := ValidateConfig(notifType, config); err != nil {
		return nil, err
	}

	now := rfc3339time.Now()
	query := `
		INSERT INTO notifications (name, type, config, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
	`

	_, err := s.db.Exec(query, name, notifType, string(config), now, now)
	if err != nil {
		return nil, fmt.Errorf("failed to create notification: %w", err)
	}

	return &Notification{
		Name:      name,
		Type:      notifType,
		Config:    config,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func (s *NotificationStore) Get(name string) (*Notification, error) {
	var n Notification
	var configStr string

	query := `SELECT name, type, config, created_at, updated_at FROM notifications WHERE name = ?`
	err := s.db.QueryRow(query, name).Scan(&n.Name, &n.Type, &configStr, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("notification %s not found", name)
		}
		return nil, fmt.Errorf("failed to get notification: %w", err)
	}

	n.Config = json.RawMessage(configStr)

	return &n, nil
}

func (s *NotificationStore) List() ([]Notification, error) {
	query := `SELECT name, type, config, created_at, updated_at FROM notifications ORDER BY name`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list notifications: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var notifications []Notification
	for rows.Next() {
		var n Notification
		var configStr string

		if err := rows.Scan(&n.Name, &n.Type, &configStr, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan notification: %w", err)
		}

		n.Config = json.RawMessage(configStr)
		notifications = append(notifications, n)
	}

	return notifications, nil
}

func (s *NotificationStore) Update(name string, notifType NotificationType, config json.RawMessage) (*Notification, error) {
	if err := ValidateNotificationType(string(notifType)); err != nil {
		return nil, err
	}
	if err := ValidateConfig(notifType, config); err != nil {
		return nil, err
	}

	now := rfc3339time.Now()
	query := `
		UPDATE notifications
		SET type = ?, config = ?, updated_at = ?
		WHERE name = ?
	`

	result, err := s.db.Exec(query, notifType, string(config), now, name)
	if err != nil {
		return nil, fmt.Errorf("failed to update notification: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("failed to check update: %w", err)
	}

	if rowsAffected == 0 {
		return nil, fmt.Errorf("notification %s not found", name)
	}

	return s.Get(name)
}

func (s *NotificationStore) Delete(name string) error {
	result, err := s.db.Exec(`DELETE FROM notifications WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("failed to delete notification: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check deletion: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("notification %s not found", name)
	}

	return nil
}

func (s *NotificationStore) LogNotification(notificationName, status string, message, errorMsg *string) error {
	now := rfc3339time.Now()
	query := `
		INSERT INTO notification_logs (notification_name, status, message, error, created_at)
		VALUES (?, ?, ?, ?, ?)
	`

	_, err := s.db.Exec(query, notificationName, status, message, errorMsg, now)
	if err != nil {
		return fmt.Errorf("failed to log notification: %w", err)
	}

	return nil
}

func (s *NotificationStore) GetLogs(limit int) ([]NotificationLog, error) {
	query := `
		SELECT id, notification_name, status, message, error, created_at 
		FROM notification_logs 
		ORDER BY created_at DESC 
		LIMIT ?
	`

	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get notification logs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var logs []NotificationLog
	for rows.Next() {
		var log NotificationLog

		if err := rows.Scan(&log.ID, &log.NotificationName, &log.Status, &log.Message, &log.Error, &log.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan log: %w", err)
		}

		logs = append(logs, log)
	}

	return logs, nil
}
