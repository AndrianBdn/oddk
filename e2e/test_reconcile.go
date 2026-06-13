package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/container"
)

// testStartupReconciliation verifies that a restarted daemon reconciles stored
// instance state with Docker reality and sweeps orphaned backup artifacts:
//   - an instance recorded "running" whose container was stopped out-of-band
//     is marked "stopped" (and can be started again),
//   - an instance recorded "stopped" whose container is actually running is
//     marked "running",
//   - stale .tmp-*/.pgpass-* artifacts in the backup dir are removed, while
//     recorded archives and unreferenced user archives survive.
func testStartupReconciliation(h *TestHarness) error {
	instanceA := testPrefix + "-reconcile-a"
	instanceB := testPrefix + "-reconcile-b"
	portA := 15438
	portB := 15439

	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("pull image: %w", err)
	}

	for name, port := range map[string]int{instanceA: portA, instanceB: portB} {
		if _, err := h.createInstanceCLI(name, port); err != nil {
			return fmt.Errorf("create instance %s: %w", name, err)
		}
		if err := h.waitForPostgreSQL(port); err != nil {
			return fmt.Errorf("wait for PostgreSQL of %s: %w", name, err)
		}
	}

	// A recorded backup whose archive must survive the sweep.
	if _, err := h.backupInstanceCLI(instanceA); err != nil {
		return fmt.Errorf("backup instance %s: %w", instanceA, err)
	}

	// Instance B: stopped via ODDK (recorded "stopped"), then its container is
	// started behind the daemon's back.
	if _, err := h.stopInstanceCLI(instanceB); err != nil {
		return fmt.Errorf("stop instance %s: %w", instanceB, err)
	}

	backupDir := filepath.Join(h.dataDir, "backups")

	// Orphaned temp artifacts as an interrupted backup/restore would leave.
	staleDirs := []string{".tmp-backup-junk-1", ".pgpass-junk"}
	for _, dir := range staleDirs {
		if err := os.MkdirAll(filepath.Join(backupDir, dir), 0o750); err != nil {
			return fmt.Errorf("create stale artifact %s: %w", dir, err)
		}
	}

	// A user-parked archive no record references: must NOT be deleted.
	unreferencedArchive := filepath.Join(backupDir, "user-parked.tar.zst")
	if err := os.WriteFile(unreferencedArchive, []byte("not a real archive"), 0o600); err != nil {
		return fmt.Errorf("create unreferenced archive: %w", err)
	}

	// Manipulate containers behind the daemon's back, then restart it.
	ctx := context.Background()
	if err := h.docker.ContainerStop(ctx, "oddk-pg-"+instanceA, container.StopOptions{}); err != nil {
		return fmt.Errorf("stop container of %s out-of-band: %w", instanceA, err)
	}
	if err := h.docker.ContainerStart(ctx, "oddk-pg-"+instanceB, container.StartOptions{}); err != nil {
		return fmt.Errorf("start container of %s out-of-band: %w", instanceB, err)
	}

	if err := h.restartDaemon(); err != nil {
		return err
	}

	// Instance A: store said "running", container is stopped -> "stopped".
	statusA, err := h.getInstanceStatusCLI(instanceA)
	if err != nil {
		return fmt.Errorf("status of %s after restart: %w", instanceA, err)
	}
	if !strings.Contains(statusA, "Status: stopped") {
		return fmt.Errorf("instance %s should be reconciled to 'stopped', got:\n%s", instanceA, statusA)
	}

	// Instance B: store said "stopped", container is running -> "running".
	statusB, err := h.getInstanceStatusCLI(instanceB)
	if err != nil {
		return fmt.Errorf("status of %s after restart: %w", instanceB, err)
	}
	if !strings.Contains(statusB, "Status: running") {
		return fmt.Errorf("instance %s should be reconciled to 'running', got:\n%s", instanceB, statusB)
	}

	// Stale artifacts swept, archives kept.
	for _, dir := range staleDirs {
		if _, err := os.Stat(filepath.Join(backupDir, dir)); !os.IsNotExist(err) {
			return fmt.Errorf("stale artifact %s should have been removed on startup", dir)
		}
	}
	if _, err := os.Stat(unreferencedArchive); err != nil {
		return fmt.Errorf("unreferenced archive should survive the startup sweep: %w", err)
	}

	backups, err := h.listBackupsCLI(instanceA)
	if err != nil {
		return fmt.Errorf("list backups after restart: %w", err)
	}
	if !strings.Contains(backups, "Local") {
		return fmt.Errorf("recorded backup of %s should survive the startup sweep, got:\n%s", instanceA, backups)
	}

	// Reconciled "stopped" is recoverable with a normal start.
	if _, err := h.startInstanceCLI(instanceA); err != nil {
		return fmt.Errorf("start %s after reconciliation: %w", instanceA, err)
	}
	statusA, err = h.getInstanceStatusCLI(instanceA)
	if err != nil {
		return fmt.Errorf("status of %s after start: %w", instanceA, err)
	}
	if !strings.Contains(statusA, "Status: running") {
		return fmt.Errorf("instance %s should be running after start, got:\n%s", instanceA, statusA)
	}

	for _, name := range []string{instanceA, instanceB} {
		if err := h.destroyInstanceCLI(name); err != nil {
			return fmt.Errorf("cleanup instance %s: %w", name, err)
		}
	}
	return nil
}
