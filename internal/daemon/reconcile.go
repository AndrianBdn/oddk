package daemon

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/hypersequent/oddk/internal/docker"
	"github.com/hypersequent/oddk/internal/store"
)

// reconcileInstances aligns each instance's stored status with the actual
// Docker container state. It runs once at daemon startup, before the HTTP
// server, schedulers, or executor accept any work, so no operation can be in
// flight. A daemon crash, a host reboot, or docker meddling while the daemon
// was down can leave SQLite claiming "running" for a stopped or missing
// container; without this pass that goes undetected until health checks fail.
// "error" is never auto-cleared — by convention only an explicit operation
// promotes out of it.
func reconcileInstances(st *store.Store, dockerClient *docker.Client) {
	instances, err := st.Instances.List()
	if err != nil {
		log.Printf("Warning: startup reconciliation skipped: list instances: %v", err)
		return
	}

	for _, inst := range instances {
		// "creating" can only mean a create operation died mid-flight.
		if inst.Status == "creating" {
			log.Printf("Reconcile: instance %s is stuck in 'creating' (interrupted create) - marking 'error'; destroy and re-create it", inst.Name)
			reconcileSetStatus(st, inst.Name, "error")
			continue
		}

		if inst.ContainerID == "" {
			if inst.Status != "error" {
				log.Printf("Reconcile: instance %s (status %q) has no container ID - marking 'error'", inst.Name, inst.Status)
				reconcileSetStatus(st, inst.Name, "error")
			}
			continue
		}

		containerState, err := dockerClient.GetContainerStatus(inst.ContainerID)
		if err != nil {
			log.Printf("Warning: reconcile: inspect container of instance %s: %v", inst.Name, err)
			continue
		}

		switch {
		case containerState == "not found":
			if inst.Status != "error" {
				log.Printf("Reconcile: container of instance %s (status %q) no longer exists - marking 'error'", inst.Name, inst.Status)
				reconcileSetStatus(st, inst.Name, "error")
			}
		case containerState == "running" && inst.Status == "stopped":
			log.Printf("Reconcile: instance %s is recorded 'stopped' but its container is running - marking 'running'", inst.Name)
			reconcileSetStatus(st, inst.Name, "running")
		case containerState == "stopped" && inst.Status == "running":
			log.Printf("Reconcile: instance %s is recorded 'running' but its container is stopped - marking 'stopped'; use 'instance start' to bring it back", inst.Name)
			reconcileSetStatus(st, inst.Name, "stopped")
		case containerState == "paused" || containerState == "restarting":
			log.Printf("Warning: reconcile: container of instance %s is %s (status %q) - leaving status unchanged", inst.Name, containerState, inst.Status)
		}
	}
}

func reconcileSetStatus(st *store.Store, name, status string) {
	if err := st.Instances.UpdateStatus(name, status); err != nil {
		log.Printf("Error: reconcile: update status of instance %s to %q: %v", name, status, err)
	}
}

// Daemon-owned temp artifacts that operations stage inside the backup
// directory. Safe to delete at startup: operations are uninterruptible and
// none can be in flight before the HTTP server starts, so anything matching
// these prefixes was orphaned by a previous daemon run.
var staleBackupArtifactPrefixes = []string{".tmp-", ".pgpass-", ".restore-", ".upgrade-"}

// sweepBackupDir removes orphaned temp artifacts from the backup directory and
// reconciles backup records with the files on disk. Records whose file is gone
// (and that have no remote copy) are cleaned up by ListAllBackups; archive
// files that no record references are only REPORTED — deleting one could
// destroy a good backup whose record write was lost, and users may park
// archives here for 'backup restore --file'.
func sweepBackupDir(st *store.Store, backupDir string) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		log.Printf("Warning: startup backup-dir sweep skipped: %v", err)
		return
	}

	removed := 0
	for _, entry := range entries {
		for _, prefix := range staleBackupArtifactPrefixes {
			if strings.HasPrefix(entry.Name(), prefix) {
				if err := os.RemoveAll(filepath.Join(backupDir, entry.Name())); err != nil {
					log.Printf("Warning: remove stale backup artifact %s: %v", entry.Name(), err)
				} else {
					removed++
				}
				break
			}
		}
	}
	if removed > 0 {
		log.Printf("Removed %d stale temp artifact(s) from backup directory (interrupted backup/restore/upgrade)", removed)
	}

	// ListAllBackups validates every record against the filesystem and deletes
	// orphaned records as a side effect; until now that only happened when
	// someone listed backups.
	records, err := st.Backup.ListAllBackups()
	if err != nil {
		log.Printf("Warning: startup backup record check skipped: %v", err)
		return
	}
	referenced := make(map[string]bool, len(records))
	for _, rec := range records {
		if rec.LocalLocation.Valid {
			referenced[filepath.Base(rec.LocalLocation.String)] = true
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".tar.zst") && !referenced[name] {
			log.Printf("Warning: backup directory contains archive %s that no backup record references - ODDK will not manage or clean it up", name)
		}
	}
}
