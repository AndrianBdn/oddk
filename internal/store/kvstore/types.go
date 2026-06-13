package kvstore

// Key represents a strongly-typed configuration key for string values
type Key string

// KeyInt represents a strongly-typed configuration key for integer values
type KeyInt string

// String configuration keys - users must use these constants
const (
	// System display name (empty = use hostname)
	KeySystemDisplayName Key = "system.display.name.str"
)

// Integer configuration keys - users must use these constants
const (
	// Health check thresholds
	KeyHealthDegradedThreshold KeyInt = "health.degraded_threshold.int"
	KeyHealthRestoredThreshold KeyInt = "health.restored_threshold.int"

	// Disk space thresholds
	KeyDiskSpaceThresholdBytes KeyInt = "health.disk_space_threshold_bytes.int"

	// CPU load thresholds (stored as percentage 0-100)
	KeyCPULoadThresholdPercent KeyInt = "health.cpu_load_threshold_percent.int"

	// Health check intervals
	KeyHealthCheckIntervalSec KeyInt = "health.check_interval_sec.int"

	// Debug settings
	KeyHealthDebugFail KeyInt = "health.debug_fail.int" // When non-zero, forces health checks to fail

	// Cron debug settings
	KeyCronDebugTickerInterval KeyInt = "cron.debug_ticker_interval.int" // Override cron ticker interval in seconds (0 = use default 60s)
	KeyCronDebugForceRun       KeyInt = "cron.debug_force_run.int"       // When 1, get all plans and force probability to 1.0

	// Backup debug settings
	KeyBackupDebugTimeMachine KeyInt = "backup.debug_time_machine.int" // When 1, enables debug endpoint to time-shift backups
)

// KVRecord represents a key-value record in the database
type KVRecord struct {
	Key       string `db:"key"`
	Value     string `db:"value"`
	UpdatedAt string `db:"updated_at"`
}

// String returns the string representation of the key
func (k Key) String() string {
	return string(k)
}

// String returns the string representation of the integer key
func (k KeyInt) String() string {
	return string(k)
}
