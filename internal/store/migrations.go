package store

import "github.com/jmoiron/sqlx"

func (s *Store) runAllMigrations() error {
	migrations := []struct {
		name string
		fn   func(*sqlx.DB) error
	}{
		{"001_initial_schema", migration001InitialSchema},
		{"002_backup_history", migration002BackupHistory},
		{"003_notifications", migration003Notifications},
		{"004_backup_comment", migration004BackupComment},
		{"005_cron_tables", migration005CronTables},
		{"006_cpu_ram_config", migration006CPURAMConfig},
		{"007_parameter_groups", migration007ParameterGroups},
		{"008_instance_parameter_groups", migration008InstanceParameterGroups},
		{"009_health_table", migration009HealthTable},
		{"010_kvstore_table", migration010KVStoreTable},
		{"011_offsite_tables", migration011OffsiteTables},
		{"012_offsite_ec2_iam_role", migration012OffsiteEC2IAMRole},
		{"013_backup_location_index", migration013BackupLocationIndex},
		{"014_backup_dual_locations", migration014BackupDualLocations},
		{"015_cron_cleanup_days", migration015CronCleanupDays},
		{"016_instance_image", migration016InstanceImage},
	}

	for _, m := range migrations {
		if err := s.runSingleMigration(m.name, m.fn); err != nil {
			return err
		}
	}
	return nil
}

func migration001InitialSchema(sqx *sqlx.DB) error {
	sqx.MustExec(`
		CREATE TABLE rdbms_instances (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			port INTEGER NOT NULL,
			version TEXT NOT NULL,
			status TEXT NOT NULL,
			container_id TEXT,
			password TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)
	`)

	sqx.MustExec(`
		CREATE TABLE auth_tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			token_prefix TEXT NOT NULL,
			token_hash TEXT UNIQUE NOT NULL,
			created_at TEXT NOT NULL
		)
	`)

	sqx.MustExec(`
		CREATE TABLE config (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)
	`)

	return nil
}

func migration002BackupHistory(sqx *sqlx.DB) error {
	sqx.MustExec(`
		CREATE TABLE backup_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			instance_name TEXT NOT NULL,
			timestamp TEXT NOT NULL,
			size INTEGER NOT NULL,
			location TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL
		)
	`)

	sqx.MustExec(`
		CREATE INDEX idx_backup_history_instance ON backup_history(instance_name)
	`)

	return nil
}

func migration003Notifications(sqx *sqlx.DB) error {
	sqx.MustExec(`
		CREATE TABLE notifications (
			name TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			config TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)
	`)

	sqx.MustExec(`
		CREATE TABLE notification_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			notification_name TEXT NOT NULL,
			status TEXT NOT NULL,
			message TEXT,
			error TEXT,
			created_at TEXT NOT NULL,
			FOREIGN KEY (notification_name) REFERENCES notifications(name) ON DELETE CASCADE
		)
	`)

	sqx.MustExec(`
		CREATE INDEX idx_notification_logs_notification_name ON notification_logs(notification_name)
	`)

	sqx.MustExec(`
		CREATE INDEX idx_notification_logs_created_at ON notification_logs(created_at)
	`)

	return nil
}

func migration004BackupComment(sqx *sqlx.DB) error {
	sqx.MustExec(`
		ALTER TABLE backup_history ADD COLUMN comment TEXT
	`)

	return nil
}

func migration005CronTables(sqx *sqlx.DB) error {
	sqx.MustExec(`
		CREATE TABLE cron_plans (
			instance_name TEXT PRIMARY KEY,
			utc_hour INTEGER NOT NULL CHECK (utc_hour >= 0 AND utc_hour < 24),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)
	`)

	sqx.MustExec(`
		CREATE TABLE cron_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			instance_name TEXT NOT NULL,
			started_at TEXT NOT NULL,
			completed_at TEXT,
			backup_status TEXT,
			backup_finished_at TEXT,
			backup_error TEXT,
			backup_upload_status TEXT,
			backup_upload_finished_at TEXT,
			backup_upload_error TEXT,
			backup_cleanup_status TEXT,
			backup_cleanup_finished_at TEXT,
			backup_cleanup_error TEXT,
			backup_remote_cleanup_status TEXT,
			backup_remote_cleanup_finished_at TEXT,
			backup_remote_cleanup_error TEXT
		)
	`)

	sqx.MustExec(`
		CREATE INDEX idx_cron_logs_instance ON cron_logs(instance_name)
	`)

	sqx.MustExec(`
		CREATE INDEX idx_cron_logs_started_at ON cron_logs(started_at)
	`)

	return nil
}

func migration006CPURAMConfig(sqx *sqlx.DB) error {
	sqx.MustExec(`
		ALTER TABLE rdbms_instances ADD COLUMN cpu_cores INTEGER NOT NULL DEFAULT 1
	`)

	sqx.MustExec(`
		ALTER TABLE rdbms_instances ADD COLUMN ram_mb INTEGER NOT NULL DEFAULT 1024
	`)

	return nil
}

func migration007ParameterGroups(sqx *sqlx.DB) error {
	sqx.MustExec(`
		CREATE TABLE parameters (
			group_name TEXT NOT NULL,
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			value_type TEXT NOT NULL,
			value TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (group_name, name)
		)
	`)

	sqx.MustExec(`
		CREATE INDEX idx_parameters_group_name ON parameters(group_name)
	`)

	return nil
}

func migration008InstanceParameterGroups(sqx *sqlx.DB) error {
	sqx.MustExec(`
		ALTER TABLE rdbms_instances ADD COLUMN parameter_group TEXT NOT NULL DEFAULT 'default:2025-08-27'
	`)

	return nil
}

func migration009HealthTable(sqx *sqlx.DB) error {
	sqx.MustExec(`
		CREATE TABLE IF NOT EXISTS health (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts_unix INTEGER NOT NULL DEFAULT (strftime('%s','now')),
			in_progress INTEGER NOT NULL DEFAULT 0,
			healthy_all INTEGER NOT NULL,
			healthy_host INTEGER NOT NULL,
			healthy_instances TEXT NOT NULL,
			broken_instances TEXT NOT NULL,
			fail_details TEXT NOT NULL
		)
	`)

	sqx.MustExec(`
		CREATE INDEX IF NOT EXISTS idx_health_ts ON health (ts_unix)
	`)

	sqx.MustExec(`
		CREATE INDEX IF NOT EXISTS idx_health_in_progress ON health (in_progress)
	`)

	return nil
}

func migration010KVStoreTable(sqx *sqlx.DB) error {
	sqx.MustExec(`
		CREATE TABLE IF NOT EXISTS kvstore (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)
	`)

	sqx.MustExec(`
		CREATE INDEX IF NOT EXISTS idx_kvstore_updated_at ON kvstore (updated_at)
	`)

	return nil
}

func migration011OffsiteTables(sqx *sqlx.DB) error {
	sqx.MustExec(`
		CREATE TABLE offsite_settings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			active INTEGER NOT NULL DEFAULT 0,
			type TEXT NOT NULL,
			bucket TEXT NOT NULL,
			endpoint TEXT,
			region TEXT,
			access_key_id TEXT NOT NULL,
			secret_access_key TEXT NOT NULL,
			bucket_path TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)
	`)

	sqx.MustExec(`
		CREATE INDEX idx_offsite_settings_active ON offsite_settings(active)
	`)

	sqx.MustExec(`
		CREATE TABLE offsite_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			event TEXT NOT NULL,
			offsite_settings_id INTEGER NOT NULL,
			object TEXT NOT NULL,
			success INTEGER NOT NULL,
			error_details TEXT,
			created_at TEXT NOT NULL,
			FOREIGN KEY (offsite_settings_id) REFERENCES offsite_settings(id)
		)
	`)

	sqx.MustExec(`
		CREATE INDEX idx_offsite_logs_offsite_settings_id ON offsite_logs(offsite_settings_id)
	`)

	sqx.MustExec(`
		CREATE INDEX idx_offsite_logs_created_at ON offsite_logs(created_at)
	`)

	return nil
}

func migration012OffsiteEC2IAMRole(sqx *sqlx.DB) error {
	sqx.MustExec(`
		ALTER TABLE offsite_settings ADD COLUMN ec2_iam_role INTEGER NOT NULL DEFAULT 0
	`)

	return nil
}

func migration013BackupLocationIndex(sqx *sqlx.DB) error {
	sqx.MustExec(`
		CREATE INDEX IF NOT EXISTS idx_backup_history_location ON backup_history(location)
	`)

	return nil
}

func migration014BackupDualLocations(sqx *sqlx.DB) error {
	// SQLite doesn't support dropping columns directly, so we need to recreate the table
	sqx.MustExec(`
		CREATE TABLE backup_history_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			instance_name TEXT NOT NULL,
			timestamp TEXT NOT NULL,
			size INTEGER NOT NULL,
			local_location TEXT,
			remote_location TEXT,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			comment TEXT,
			CHECK (local_location IS NOT NULL OR remote_location IS NOT NULL)
		)
	`)

	// Migrate existing data
	sqx.MustExec(`
		INSERT INTO backup_history_new (id, instance_name, timestamp, size, local_location, remote_location, status, created_at, comment)
		SELECT id, instance_name, timestamp, size,
			CASE WHEN location NOT LIKE 's3://%' THEN location ELSE NULL END,
			CASE WHEN location LIKE 's3://%' THEN location ELSE NULL END,
			status, created_at, comment
		FROM backup_history
	`)

	// Drop old table and rename new one
	sqx.MustExec(`DROP TABLE backup_history`)
	sqx.MustExec(`ALTER TABLE backup_history_new RENAME TO backup_history`)

	// Recreate indexes
	sqx.MustExec(`CREATE INDEX idx_backup_history_instance ON backup_history(instance_name)`)
	sqx.MustExec(`CREATE INDEX idx_backup_history_local_location ON backup_history(local_location)`)
	sqx.MustExec(`CREATE INDEX idx_backup_history_remote_location ON backup_history(remote_location)`)

	return nil
}

func migration016InstanceImage(sqx *sqlx.DB) error {
	sqx.MustExec(`ALTER TABLE rdbms_instances ADD COLUMN image TEXT NOT NULL DEFAULT ''`)
	sqx.MustExec(`UPDATE rdbms_instances SET image = 'postgres:' || version WHERE image = ''`)
	return nil
}

func migration015CronCleanupDays(sqx *sqlx.DB) error {
	// SQLite doesn't support column reordering directly, so we need to recreate the table
	// with the columns in the desired order
	sqx.MustExec(`
		CREATE TABLE cron_plans_new (
			instance_name TEXT PRIMARY KEY,
			utc_hour INTEGER NOT NULL CHECK (utc_hour >= 0 AND utc_hour < 24),
			cleanup_local_days INTEGER NOT NULL DEFAULT 7,
			cleanup_remote_days INTEGER NOT NULL DEFAULT 14,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)
	`)

	// Copy existing data to the new table
	sqx.MustExec(`
		INSERT INTO cron_plans_new (instance_name, utc_hour, cleanup_local_days, cleanup_remote_days, created_at, updated_at)
		SELECT instance_name, utc_hour, 7, 14, created_at, updated_at
		FROM cron_plans
	`)

	// Drop the old table and rename the new one
	sqx.MustExec(`DROP TABLE cron_plans`)
	sqx.MustExec(`ALTER TABLE cron_plans_new RENAME TO cron_plans`)

	return nil
}
