package cron

import "github.com/hypersequent/oddk/internal/rfc3339time"

type CronPlan struct {
	InstanceName      string           `db:"instance_name" json:"instanceName"`
	UTCHour           int              `db:"utc_hour" json:"utcHour"`
	CleanupLocalDays  int              `db:"cleanup_local_days" json:"cleanupLocalDays"`
	CleanupRemoteDays int              `db:"cleanup_remote_days" json:"cleanupRemoteDays"`
	CreatedAt         rfc3339time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt         rfc3339time.Time `db:"updated_at" json:"updatedAt"`
}

type CronLog struct {
	ID                            int               `db:"id" json:"id,omitempty"`
	InstanceName                  string            `db:"instance_name" json:"instanceName"`
	StartedAt                     rfc3339time.Time  `db:"started_at" json:"startedAt"`
	CompletedAt                   *rfc3339time.Time `db:"completed_at" json:"completedAt,omitempty"`
	BackupStatus                  *string           `db:"backup_status" json:"backupStatus,omitempty"`
	BackupFinishedAt              *rfc3339time.Time `db:"backup_finished_at" json:"backupFinishedAt,omitempty"`
	BackupError                   *string           `db:"backup_error" json:"backupError,omitempty"`
	BackupUploadStatus            *string           `db:"backup_upload_status" json:"backupUploadStatus,omitempty"`
	BackupUploadFinishedAt        *rfc3339time.Time `db:"backup_upload_finished_at" json:"backupUploadFinishedAt,omitempty"`
	BackupUploadError             *string           `db:"backup_upload_error" json:"backupUploadError,omitempty"`
	BackupCleanupStatus           *string           `db:"backup_cleanup_status" json:"backupCleanupStatus,omitempty"`
	BackupCleanupFinishedAt       *rfc3339time.Time `db:"backup_cleanup_finished_at" json:"backupCleanupFinishedAt,omitempty"`
	BackupCleanupError            *string           `db:"backup_cleanup_error" json:"backupCleanupError,omitempty"`
	BackupRemoteCleanupStatus     *string           `db:"backup_remote_cleanup_status" json:"backupRemoteCleanupStatus,omitempty"`
	BackupRemoteCleanupFinishedAt *rfc3339time.Time `db:"backup_remote_cleanup_finished_at" json:"backupRemoteCleanupFinishedAt,omitempty"`
	BackupRemoteCleanupError      *string           `db:"backup_remote_cleanup_error" json:"backupRemoteCleanupError,omitempty"`
}
