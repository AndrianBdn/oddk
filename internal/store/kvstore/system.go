package kvstore

import (
	"fmt"
	"log"
	"os"
)

// Default system parameters
const (
	// System display name (empty string = use hostname)
	defaultSystemDisplayName = ""

	// Health check thresholds (number of failures/successes)
	defaultHealthDegradedThreshold = 3
	defaultHealthRestoredThreshold = 2

	// Disk space thresholds (in bytes) - 1GB default
	defaultDiskSpaceThresholdBytes = 1024 * 1024 * 1024

	// CPU load thresholds (stored as percentage 0-100) - 95% default
	defaultCPULoadThresholdPercent = 95

	// Health check intervals (in seconds) - 70 seconds default
	defaultHealthCheckIntervalSec = 70

	// Debug fail mode - 0 (disabled) by default
	defaultHealthDebugFail = 0

	// Cron debug settings - 0 (disabled) by default
	defaultCronDebugTickerInterval = 0
	defaultCronDebugForceRun       = 0
)

// systemIntDefaults maps every required integer system parameter to its
// default, for RequiredInt's fallback. Keep in sync with EnsureSystemDefaults.
var systemIntDefaults = map[KeyInt]int{
	KeyHealthDegradedThreshold: defaultHealthDegradedThreshold,
	KeyHealthRestoredThreshold: defaultHealthRestoredThreshold,
	KeyDiskSpaceThresholdBytes: defaultDiskSpaceThresholdBytes,
	KeyCPULoadThresholdPercent: defaultCPULoadThresholdPercent,
	KeyHealthCheckIntervalSec:  defaultHealthCheckIntervalSec,
	KeyHealthDebugFail:         defaultHealthDebugFail,
	KeyCronDebugTickerInterval: defaultCronDebugTickerInterval,
	KeyCronDebugForceRun:       defaultCronDebugForceRun,
}

// RequiredInt returns the value of a required integer system parameter.
// Parameters are seeded by EnsureSystemDefaults, so a miss means the kvstore
// row was deleted or corrupted; rather than crashing the daemon (this is
// called from long-running paths like health checks), it logs loudly and
// falls back to the registered default.
func (kv *KVStore) RequiredInt(key KeyInt) int {
	value, err := kv.GetInt(key)
	if err == nil {
		return value
	}
	def := systemIntDefaults[key]
	log.Printf("Error: required system parameter %s unreadable (%v) - using default %d", key.String(), err, def)
	return def
}

// EnsureSystemDefaults ensures all required system parameters exist with sensible defaults
// This should be called during Initialize() to populate missing system parameters
func (kv *KVStore) EnsureSystemDefaults() error {
	// Ensure system display name exists
	if !kv.ExistsRaw(KeySystemDisplayName.String()) {
		if err := kv.Set(KeySystemDisplayName, defaultSystemDisplayName); err != nil {
			return err
		}
	}

	// Ensure health check thresholds exist
	if !kv.ExistsRaw(KeyHealthDegradedThreshold.String()) {
		if err := kv.SetInt(KeyHealthDegradedThreshold, defaultHealthDegradedThreshold); err != nil {
			return err
		}
	}

	if !kv.ExistsRaw(KeyHealthRestoredThreshold.String()) {
		if err := kv.SetInt(KeyHealthRestoredThreshold, defaultHealthRestoredThreshold); err != nil {
			return err
		}
	}

	// Ensure disk space threshold exists
	if !kv.ExistsRaw(KeyDiskSpaceThresholdBytes.String()) {
		if err := kv.SetInt(KeyDiskSpaceThresholdBytes, defaultDiskSpaceThresholdBytes); err != nil {
			return err
		}
	}

	// Ensure CPU load threshold exists
	if !kv.ExistsRaw(KeyCPULoadThresholdPercent.String()) {
		if err := kv.SetInt(KeyCPULoadThresholdPercent, defaultCPULoadThresholdPercent); err != nil {
			return err
		}
	}

	// Ensure health check interval exists
	if !kv.ExistsRaw(KeyHealthCheckIntervalSec.String()) {
		if err := kv.SetInt(KeyHealthCheckIntervalSec, defaultHealthCheckIntervalSec); err != nil {
			return err
		}
	}

	// Ensure debug fail mode exists (disabled by default)
	if !kv.ExistsRaw(KeyHealthDebugFail.String()) {
		if err := kv.SetInt(KeyHealthDebugFail, defaultHealthDebugFail); err != nil {
			return err
		}
	}

	// Ensure cron debug ticker interval exists (disabled by default)
	if !kv.ExistsRaw(KeyCronDebugTickerInterval.String()) {
		if err := kv.SetInt(KeyCronDebugTickerInterval, defaultCronDebugTickerInterval); err != nil {
			return err
		}
	}

	// Ensure cron debug force run exists (disabled by default)
	if !kv.ExistsRaw(KeyCronDebugForceRun.String()) {
		if err := kv.SetInt(KeyCronDebugForceRun, defaultCronDebugForceRun); err != nil {
			return err
		}
	}

	return nil
}

// getHostname returns the system hostname
func getHostname() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("get hostname: %w", err)
	}
	return hostname, nil
}

// GetDisplayName returns the system display name or hostname if empty
func (kv *KVStore) GetDisplayName() string {
	displayName := kv.GetWithDefault(KeySystemDisplayName, "")

	if displayName == "" {
		// Fallback to hostname
		hostname, err := getHostname()
		if err != nil {
			return "unknown"
		}
		return hostname
	}

	return displayName
}
