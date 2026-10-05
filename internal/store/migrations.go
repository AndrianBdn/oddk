package store

import (
	"fmt"
	"log"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/andrianbdn/oddk/internal/store/instances"
)

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
		{"017_snapshot_tables", migration017SnapshotTables},
		{"018_snapshot_format", migration018SnapshotFormat},
		{"019_snapshot_instances", migration019SnapshotInstances},
		{"020_instance_status_check", migration020InstanceStatusCheck},
		{"021_schedule_pause", migration021SchedulePause},
		{"022_archive_sha256", migration022ArchiveSHA256},
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

// migration017SnapshotTables adds the deployment-wide snapshot schedule and the
// catalogue of snapshots taken.
//
// snapshot_plans is a SINGLETON — `id INTEGER PRIMARY KEY CHECK (id = 1)` — not
// a per-instance table like cron_plans. A snapshot covers the whole deployment,
// so per-instance scheduling would be expressing something that cannot happen.
// The CHECK is what enforces it: without it, nothing stops a second plan row and
// the scheduler would silently pick whichever it read first.
//
// interval_hours is anchored to utc_hour, so a plan runs at every hour h where
// (h - utc_hour) mod interval_hours == 0. It is constrained to a divisor of 24
// so the pattern does not go ragged across midnight: interval 5 anchored at 03
// would fire 03,08,13,18,23 and then 03 again — a 4-hour gap that silently
// breaks the "every 5 hours" the operator asked for. Divisors are 1,2,3,4,6,8,
// 12,24; 24 (the default) means once a day at utc_hour, matching cron_plans.
//
// snapshot_history mirrors backup_history's dual-location model, including its
// CHECK: a row with neither a local nor a remote copy describes nothing, and
// retention must delete the row rather than leave a phantom.
func migration017SnapshotTables(sqx *sqlx.DB) error {
	sqx.MustExec(`
		CREATE TABLE snapshot_plans (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			utc_hour INTEGER NOT NULL CHECK (utc_hour >= 0 AND utc_hour < 24),
			interval_hours INTEGER NOT NULL DEFAULT 24
				CHECK (interval_hours IN (1, 2, 3, 4, 6, 8, 12, 24)),
			cleanup_local_days INTEGER NOT NULL DEFAULT 7,
			cleanup_remote_days INTEGER NOT NULL DEFAULT 14,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)
	`)

	sqx.MustExec(`
		CREATE TABLE snapshot_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			filename TEXT NOT NULL,
			created_at TEXT NOT NULL,
			size INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL,
			instances_with_data INTEGER NOT NULL DEFAULT 0,
			config_only INTEGER NOT NULL DEFAULT 0,
			local_location TEXT,
			remote_location TEXT,
			comment TEXT,
			CHECK (local_location IS NOT NULL OR remote_location IS NOT NULL)
		)
	`)

	sqx.MustExec(`CREATE INDEX idx_snapshot_history_created_at ON snapshot_history(created_at)`)

	return nil
}

// migration018SnapshotFormat records each snapshot's and the schedule's format:
// "physical" (pg_basebackup, the default since 0.1.61) or "logical" (the
// original pg_dump archives).
//
// The two DEFAULTs point in different directions ON PURPOSE. An existing PLAN
// backfills to 'physical' — that is the "snapshots become binary by default"
// switch, applied to already-scheduled deployments on upgrade. Existing
// HISTORY rows backfill to 'logical' because that is simply what every
// pre-0.1.61 archive is.
func migration018SnapshotFormat(sqx *sqlx.DB) error {
	sqx.MustExec(`
		ALTER TABLE snapshot_plans ADD COLUMN format TEXT NOT NULL DEFAULT 'physical'
			CHECK (format IN ('physical', 'logical'))
	`)
	sqx.MustExec(`
		ALTER TABLE snapshot_history ADD COLUMN format TEXT NOT NULL DEFAULT 'logical'
			CHECK (format IN ('physical', 'logical'))
	`)

	return nil
}

// migration019SnapshotInstances records WHICH instances each snapshot holds and
// whether each was captured with data, as a JSON array of {"name","hasData"}.
//
// The counts added in 017 (instances_with_data, config_only) cannot answer the
// question the checklist audit asks per instance: "is THIS instance's data in
// the newest snapshot?" — a configuration-only entry (stopped under logical;
// vanished/error container under physical) was indistinguishable from a real
// capture, so the audit credited coverage that a restore could not deliver.
//
// Nullable with no default: NULL marks a pre-019 row, for which the checklist
// falls back to the optimistic created-at heuristic rather than pretending to
// know what an old archive holds.
func migration019SnapshotInstances(sqx *sqlx.DB) error {
	sqx.MustExec(`ALTER TABLE snapshot_history ADD COLUMN instances_json TEXT`)
	return nil
}

// migration020InstanceStatusCheck puts a CHECK constraint on
// rdbms_instances.status.
//
// The column was TEXT NOT NULL with no constraint and no Go type, written as
// bare string literals from a dozen sites — and it had quietly become a
// DATA-SAFETY input: snapshot capture, the health checker and every data command
// branch on it, so a value no reader understood meant an instance silently
// dropped out of monitoring and out of DR archives. The Go side now owns the
// vocabulary (internal/store/instances/status.go); this is the database half, so
// a writer that bypasses that package fails loudly here instead of storing
// something nothing can interpret.
//
// SQLite cannot ALTER TABLE ADD CONSTRAINT, so this is the standard table
// rebuild — wrapped in ONE transaction, because a failure between DROP and
// RENAME would leave a deployment with no instances table at all. There are no
// foreign keys referencing rdbms_instances, so no PRAGMA dance is needed.
//
// Rows carrying an unrecognised status are normalized to 'error' during the copy
// rather than failing the migration: refusing to start the daemon because of a
// stale status value would be a far worse outcome than quarantining that one
// instance, and 'error' is what startup reconciliation would conclude anyway.
// The legacy broken/broken-port/broken-auth values are ACCEPTED (see the note on
// their constants) so already-affected rows survive to be un-latched by
// reconciliation on this same startup; nothing writes them any more.
func migration020InstanceStatusCheck(sqx *sqlx.DB) error {
	quoted := make([]string, 0, len(instances.AllStatuses()))
	for _, s := range instances.AllStatuses() {
		quoted = append(quoted, "'"+string(s)+"'")
	}
	allowed := strings.Join(quoted, ", ")

	tx, err := sqx.Beginx()
	if err != nil {
		return fmt.Errorf("020: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	if _, err := tx.Exec(fmt.Sprintf(`
		CREATE TABLE rdbms_instances_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			port INTEGER NOT NULL,
			version TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN (%s)),
			container_id TEXT,
			password TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			cpu_cores INTEGER NOT NULL DEFAULT 1,
			ram_mb INTEGER NOT NULL DEFAULT 1024,
			parameter_group TEXT NOT NULL DEFAULT 'default:2025-08-27',
			image TEXT NOT NULL DEFAULT ''
		)`, allowed)); err != nil {
		return fmt.Errorf("020: create table: %w", err)
	}

	// Count what we are about to quarantine, so the operator can be told rather
	// than discovering an instance in 'error' with no explanation.
	var normalized int
	if err := tx.Get(&normalized,
		fmt.Sprintf(`SELECT COUNT(*) FROM rdbms_instances WHERE status NOT IN (%s)`, allowed)); err != nil {
		return fmt.Errorf("020: count unrecognised statuses: %w", err)
	}

	if _, err := tx.Exec(fmt.Sprintf(`
		INSERT INTO rdbms_instances_new
			(id, name, port, version, status, container_id, password,
			 created_at, updated_at, cpu_cores, ram_mb, parameter_group, image)
		SELECT
			id, name, port, version,
			CASE WHEN status IN (%s) THEN status ELSE 'error' END,
			container_id, password,
			created_at, updated_at, cpu_cores, ram_mb, parameter_group, image
		FROM rdbms_instances`, allowed)); err != nil {
		return fmt.Errorf("020: copy rows: %w", err)
	}

	// Prove the copy is complete before dropping the original. A silently short
	// copy here would delete a deployment's entire instance registry, including
	// the encrypted postgres passwords, which nothing else holds.
	var oldCount, newCount int
	if err := tx.Get(&oldCount, `SELECT COUNT(*) FROM rdbms_instances`); err != nil {
		return fmt.Errorf("020: count source rows: %w", err)
	}
	if err := tx.Get(&newCount, `SELECT COUNT(*) FROM rdbms_instances_new`); err != nil {
		return fmt.Errorf("020: count copied rows: %w", err)
	}
	if oldCount != newCount {
		return fmt.Errorf("020: copied %d of %d instance rows; refusing to drop the original", newCount, oldCount)
	}

	if _, err := tx.Exec(`DROP TABLE rdbms_instances`); err != nil {
		return fmt.Errorf("020: drop old table: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE rdbms_instances_new RENAME TO rdbms_instances`); err != nil {
		return fmt.Errorf("020: rename: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("020: commit: %w", err)
	}

	if normalized > 0 {
		log.Printf("Migration 020: %d instance(s) had an unrecognised status and were marked 'error'; "+
			"use 'oddk instance start <name>' to bring one back", normalized)
	}
	return nil
}

// migration021SchedulePause lets a schedule be SUSPENDED without being deleted.
//
// The case that forced it: `oddk snapshot apply` restores the source host's
// oddk.db, which carries that host's snapshot schedule AND its offsite
// settings. On a DR rehearsal or a planned migration — where the SOURCE IS
// STILL LIVE — the restored host then starts writing snapshots into the same
// bucket and, worse, running offsite RETENTION against it, deleting objects the
// source still catalogues. The documented workaround was `setup-cron --remove`,
// which throws the schedule away: a real DR host whose operator forgets to
// recreate it is then permanently unprotected, trading a rehearsal hazard for a
// production one.
//
// Pausing keeps the schedule and its settings intact, is reversible with one
// command, and is VISIBLE — `oddk checklist` reports a paused schedule as a
// problem, not as "not scheduled".
//
// paused_at is nullable because NULL is the overwhelmingly common state and
// means exactly one thing: this schedule runs. paused_reason is NOT NULL
// DEFAULT ” so it scans into a plain string; it records WHO paused it and why,
// which is what distinguishes "an operator paused this deliberately" from
// "a DR restore paused it for you and you have not resumed it yet".
func migration021SchedulePause(sqx *sqlx.DB) error {
	for _, table := range []string{"snapshot_plans", "cron_plans"} {
		sqx.MustExec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN paused_at TEXT`, table))
		sqx.MustExec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN paused_reason TEXT NOT NULL DEFAULT ''`, table))
	}
	return nil
}

// migration022ArchiveSHA256 records the SHA-256 of each archive file, computed
// in the read-back that already verifies every archive before it is published.
//
// Nullable, and NULL means "unknown" (a row written before this migration), never
// "no digest expected". The zstd frame checksum already says whether ONE copy is
// internally broken; the stored digest is what says whether two intact copies —
// local, S3, a DR host's download — are the same archive.
func migration022ArchiveSHA256(sqx *sqlx.DB) error {
	for _, table := range []string{"snapshot_history", "backup_history"} {
		sqx.MustExec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN sha256 TEXT`, table))
	}
	return nil
}
