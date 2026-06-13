package services

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/volume"
	"github.com/jackc/pgx/v5"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"

	"github.com/hypersequent/oddk/internal/docker"
	"github.com/hypersequent/oddk/internal/store"
	"github.com/hypersequent/oddk/internal/store/instances"
	"github.com/hypersequent/oddk/internal/store/kvstore"
)

type connectionEntry struct {
	conn     *pgx.Conn
	lastUsed time.Time
}

type HealthChecker struct {
	store     *store.Store
	docker    *docker.Client
	dataDir   string
	backupDir string
	masterKey []byte

	// Cached connections for PostgreSQL health checks
	// Map: instance_name -> {connection, lastUsed}
	// Keyed by instance name so destroy/recreate on the same port can't reuse a stale conn.
	// connMutex is held for the entire getOrCreateConnection call (including the pgx
	// dial/ping). Health checks run sequentially today and dials are sub-second on the
	// 10.88.0.0/16 network, so the simpler lock buys race-freedom at negligible cost.
	connections map[string]*connectionEntry
	connMutex   sync.Mutex

	// CPU load tracking (ring buffer)
	cpuSamples     []float64
	cpuSampleIndex int
	cpuSampleCount int

	// Notification evaluator for health status notifications
	notificationEvaluator *HealthNotificationEvaluator
}

type HostCheckResult struct {
	DockerOK    bool
	DiskSpaceOK bool
	CPUOK       bool
	FailMsg     string
}

type InstanceHealthResult struct {
	HealthyInstances []string
	BrokenInstances  []string
	FailMsg          string
}

func NewHealthChecker(store *store.Store, docker *docker.Client, dataDir, backupDir string, masterKey []byte) *HealthChecker {
	return &HealthChecker{
		store:                 store,
		docker:                docker,
		dataDir:               dataDir,
		backupDir:             backupDir,
		masterKey:             masterKey,
		connections:           make(map[string]*connectionEntry),
		cpuSamples:            make([]float64, 5), // Last 5 samples for averaging
		notificationEvaluator: NewHealthNotificationEvaluator(store),
	}
}

// RunHealthCheck performs a complete health check cycle
func (hc *HealthChecker) RunHealthCheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	record, err := hc.store.Health.StartHealthCheck()
	if err != nil {
		return fmt.Errorf("start health check: %w", err)
	}

	// Perform host checks
	hostResult := hc.performHostChecks(ctx)

	// Perform instance checks
	instanceResult := hc.performInstanceChecks(ctx)

	// Concatenate all fail messages
	var failMsgs []string
	if hostResult.FailMsg != "" {
		failMsgs = append(failMsgs, hostResult.FailMsg)
	}
	if instanceResult.FailMsg != "" {
		failMsgs = append(failMsgs, instanceResult.FailMsg)
	}
	failMsg := strings.Join(failMsgs, ",")

	err = hc.store.Health.UpdateHealthCheck(
		record.ID,
		hostResult.isHealthy(),
		instanceResult.HealthyInstances,
		instanceResult.BrokenInstances,
		failMsg,
	)
	if err != nil {
		return fmt.Errorf("update health check: %w", err)
	}

	// Evaluate health status and send notifications if thresholds are met
	if err := hc.notificationEvaluator.EvaluateAndNotify(ctx); err != nil {
		log.Printf("Error evaluating health notifications: %v", err)
	}

	// Clean up old records (3 days retention)
	_ = hc.store.Health.CleanupOldRecords(3 * 24 * time.Hour)

	// Clean up stale connections (10 minutes unused)
	hc.cleanupStaleConnections(10 * time.Minute)

	return nil
}

// ResetInProgressRecords resets any stuck in_progress records on startup
func (hc *HealthChecker) ResetInProgressRecords() error {
	return hc.store.Health.ResetInProgressRecords()
}

func (hr *HostCheckResult) isHealthy() bool {
	return hr.DockerOK && hr.DiskSpaceOK && hr.CPUOK
}

// performHostChecks executes all host-level health checks
func (hc *HealthChecker) performHostChecks(ctx context.Context) HostCheckResult {
	result := HostCheckResult{
		DockerOK:    true,
		DiskSpaceOK: true,
		CPUOK:       true,
	}

	// Check if debug fail mode is enabled
	debugFail := hc.store.KV.RequiredInt(kvstore.KeyHealthDebugFail)
	if debugFail != 0 {
		result.DockerOK = false
		result.DiskSpaceOK = false
		result.CPUOK = false
		result.FailMsg = "debug_fail_enabled"
		return result // Force failure for debugging
	}

	// 1. Docker daemon check
	if !hc.checkDocker(ctx) {
		result.DockerOK = false
		result.FailMsg = "docker_unreachable"
		return result // Fail fast
	}

	// 2. Discover paths and devices, check free space
	if failMsg := hc.checkDiskSpace(ctx); failMsg != "" {
		result.DiskSpaceOK = false
		result.FailMsg = failMsg
		return result // Fail fast on first disk issue
	}

	// 3. CPU load check
	if !hc.checkCPULoad() {
		result.CPUOK = false
		result.FailMsg = "cpu_sustained_high"
		return result
	}

	return result
}

// checkDocker verifies Docker daemon is reachable
func (hc *HealthChecker) checkDocker(ctx context.Context) bool {
	dockerClient := hc.docker.GetDockerClient()

	// Set a reasonable timeout for docker info
	infoCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := dockerClient.Info(infoCtx)
	return err == nil
}

// checkDiskSpace checks free space on all relevant paths
func (hc *HealthChecker) checkDiskSpace(ctx context.Context) string {
	var lowSpaceAreas []string

	thresholdBytes := int64(hc.store.KV.RequiredInt(kvstore.KeyDiskSpaceThresholdBytes))

	if free, err := hc.getFreeSpaceForPath(hc.backupDir); err == nil && free < thresholdBytes {
		lowSpaceAreas = append(lowSpaceAreas, "backup")
	}

	if free, err := hc.getFreeSpaceForPath(hc.dataDir); err == nil && free < thresholdBytes {
		lowSpaceAreas = append(lowSpaceAreas, "dbdir")
	}

	volumeMounts, err := hc.getDockerVolumeMountpoints(ctx)
	if err == nil {
		for _, mountpoint := range volumeMounts {
			if free, err := hc.getFreeSpaceForPath(mountpoint); err == nil && free < thresholdBytes {
				lowSpaceAreas = append(lowSpaceAreas, "volumes")
				break // Only report volumes once, not per mountpoint
			}
		}
	}

	if len(lowSpaceAreas) == 0 {
		return ""
	}

	return "low_space:" + strings.Join(lowSpaceAreas, ",")
}

// getDockerVolumeMountpoints retrieves mount points for Docker volumes
func (hc *HealthChecker) getDockerVolumeMountpoints(ctx context.Context) ([]string, error) {
	dockerClient := hc.docker.GetDockerClient()

	volumes, err := dockerClient.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, err
	}

	var mountpoints []string
	for _, vol := range volumes.Volumes {
		if vol.Mountpoint != "" {
			mountpoints = append(mountpoints, vol.Mountpoint)
		}
	}

	return mountpoints, nil
}

// getFreeSpaceForPath returns free bytes for a specific path
func (hc *HealthChecker) getFreeSpaceForPath(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	// Free blocks * block size
	// #nosec G115 - filesystem size calculation, overflow extremely unlikely
	return int64(stat.Bavail * uint64(stat.Bsize)), nil
}

// checkCPULoad checks sustained high CPU usage
func (hc *HealthChecker) checkCPULoad() bool {
	// Add current CPU sample
	currentCPU := hc.getCurrentCPUUsage()
	hc.addCPUSample(currentCPU)

	// Check if we have enough samples
	if hc.cpuSampleCount < 5 {
		return true // Not enough samples yet, assume healthy
	}

	// Get configurable CPU threshold (stored as percentage 0-100)
	thresholdPercent := float64(hc.store.KV.RequiredInt(kvstore.KeyCPULoadThresholdPercent))

	// Calculate average
	avg := hc.getAverageCPU()
	return avg < thresholdPercent
}

// getCurrentCPUUsage returns current CPU load average as a percentage
func (hc *HealthChecker) getCurrentCPUUsage() float64 {
	avgStat, err := load.Avg()
	if err != nil {
		return 0.0 // Assume healthy if we can't read
	}

	numCPU, err := cpu.Counts(true) // logical cores (includes hyperthreading)
	if err != nil || numCPU <= 0 {
		numCPU = 1 // Fallback to prevent division by zero
	}

	// Use 1-minute load average normalized by CPU count
	// Load average of 1.0 per core = 100% utilization
	// Convert to percentage: (load / cores) * 100
	return (avgStat.Load1 / float64(numCPU)) * 100.0
}

// addCPUSample adds a CPU sample to the ring buffer
func (hc *HealthChecker) addCPUSample(sample float64) {
	hc.cpuSamples[hc.cpuSampleIndex] = sample
	hc.cpuSampleIndex = (hc.cpuSampleIndex + 1) % len(hc.cpuSamples)
	if hc.cpuSampleCount < len(hc.cpuSamples) {
		hc.cpuSampleCount++
	}
}

// getAverageCPU returns the average of collected CPU samples
func (hc *HealthChecker) getAverageCPU() float64 {
	if hc.cpuSampleCount == 0 {
		return 0.0
	}

	sum := 0.0
	count := hc.cpuSampleCount
	for i := range count {
		sum += hc.cpuSamples[i]
	}

	return sum / float64(count)
}

// performInstanceChecks checks health of all PostgreSQL instances
func (hc *HealthChecker) performInstanceChecks(ctx context.Context) InstanceHealthResult {
	result := InstanceHealthResult{
		HealthyInstances: []string{},
		BrokenInstances:  []string{},
	}

	instances, err := hc.store.Instances.List()
	if err != nil {
		result.FailMsg = fmt.Sprintf("runtime_error:list_%s", shortenError(err.Error()))
		return result
	}

	var failMessages []string
	for _, instance := range instances {
		// Get fresh instance data to avoid race conditions during iteration
		freshInstance, err := hc.store.Instances.Get(instance.Name)
		if err != nil {
			// Instance may have been deleted while we were iterating, skip it
			continue
		}

		if freshInstance.Status != "running" {
			continue // Skip stopped instances
		}

		if hc.checkInstanceHealth(ctx, freshInstance) {
			result.HealthyInstances = append(result.HealthyInstances, freshInstance.Name)
		} else {
			result.BrokenInstances = append(result.BrokenInstances, freshInstance.Name)
			// Collect all failure messages
			failMessages = append(failMessages, fmt.Sprintf("pg_ping_failed:%s", freshInstance.Name))
		}
	}

	// Concatenate all failure messages
	if len(failMessages) > 0 {
		result.FailMsg = strings.Join(failMessages, ",")
	}

	return result
}

// getOrCreateConnection gets a cached connection for the named instance or creates a new one.
// Cached entries are keyed by instance name; if the connection is dead it is replaced.
//
// The cache lock is held for the entire call — including the pgx Ping and Connect.
// This serializes connection lookup across instances, but health checks already run
// sequentially in a single goroutine and dials are sub-second, so the cost is
// negligible and the simpler lock eliminates the TOCTOU race against
// cleanupStaleConnections / CloseInstanceConnections.
func (hc *HealthChecker) getOrCreateConnection(ctx context.Context, instanceName, connURL string) (*pgx.Conn, error) {
	hc.connMutex.Lock()
	defer hc.connMutex.Unlock()

	if entry, exists := hc.connections[instanceName]; exists && entry.conn != nil {
		pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()

		if err := entry.conn.Ping(pingCtx); err == nil {
			entry.lastUsed = time.Now()
			return entry.conn, nil
		}

		// Connection is dead, close and drop it before redialing.
		_ = entry.conn.Close(ctx)
		delete(hc.connections, instanceName)
	}

	conn, err := pgx.Connect(ctx, connURL)
	if err != nil {
		return nil, err
	}

	hc.connections[instanceName] = &connectionEntry{
		conn:     conn,
		lastUsed: time.Now(),
	}
	return conn, nil
}

// cleanupStaleConnections closes connections that haven't been used for a while
func (hc *HealthChecker) cleanupStaleConnections(maxAge time.Duration) {
	hc.connMutex.Lock()
	defer hc.connMutex.Unlock()

	cutoff := time.Now().Add(-maxAge)
	for name, entry := range hc.connections {
		if entry.lastUsed.Before(cutoff) {
			if entry.conn != nil {
				_ = entry.conn.Close(context.Background())
			}
			delete(hc.connections, name)
		}
	}
}

// CloseInstanceConnections closes the cached connection for a specific instance.
// Called when an instance is stopped, destroyed, or about to be reconfigured.
func (hc *HealthChecker) CloseInstanceConnections(instanceName string) {
	hc.connMutex.Lock()
	defer hc.connMutex.Unlock()

	if entry, exists := hc.connections[instanceName]; exists {
		if entry.conn != nil {
			_ = entry.conn.Close(context.Background())
		}
		delete(hc.connections, instanceName)
	}
}

// checkInstanceHealth checks if a PostgreSQL instance is healthy
func (hc *HealthChecker) checkInstanceHealth(ctx context.Context, instance *instances.RDBMSInstance) bool {
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// Decrypt the password
	password, err := hc.store.Instances.GetDecryptedPassword(instance.Name, hc.masterKey)
	if err != nil {
		return false
	}

	// Build connection string using the correct pattern with sslmode=disable
	connStr := fmt.Sprintf("postgres://postgres:%s@10.88.0.1:%d/postgres?sslmode=disable",
		password, instance.Port)

	// Get or create cached connection
	// getOrCreateConnection already validates the connection with ping, so no need to ping again
	_, err = hc.getOrCreateConnection(checkCtx, instance.Name, connStr)
	if err != nil {
		return false
	}
	// Note: Don't close the connection here since it's cached

	return true
}

// Helper function to shorten error messages for fail_details
func shortenError(err string) string {
	if len(err) > 20 {
		return err[:20]
	}
	return err
}
