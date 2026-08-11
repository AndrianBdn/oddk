package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operations"
	"github.com/andrianbdn/oddk/internal/store"
	"github.com/andrianbdn/oddk/internal/store/instances"
)

// reconcileInstances aligns each instance's stored status with the actual
// Docker container state. It runs once at daemon startup, before the HTTP
// server, schedulers, or executor accept any work, so no operation can be in
// flight. A daemon crash, a host reboot, or docker meddling while the daemon
// was down can leave SQLite claiming "running" for a stopped or missing
// container; without this pass that goes undetected until health checks fail.
// "error" is never auto-cleared — by convention only an explicit operation
// promotes out of it.
//
// The pass also re-attaches any existing instance container to oddk-bridge if
// it isn't attached (see the inline comment for why that matters beyond the
// single instance).
func reconcileInstances(st *store.Store, dockerClient *docker.Client) {
	list, err := st.Instances.List()
	if err != nil {
		log.Printf("Warning: startup reconciliation skipped: list instances: %v", err)
		return
	}

	for _, inst := range list {
		containerState := ""
		if inst.ContainerID != "" {
			state, err := dockerClient.GetContainerStatus(inst.ContainerID)
			if err != nil {
				log.Printf("Warning: reconcile: inspect container of instance %s: %v", inst.Name, err)
				continue
			}
			containerState = state

			// A container recreated outside ODDK (e.g. manual disaster
			// recovery) can end up on the default bridge only. Besides breaking
			// 10.88.0.1 routing for that instance, it leaves oddk-bridge with
			// zero attached containers — which makes the network eligible for
			// 'docker network prune' and takes down every instance at once.
			// Re-attaching here restores the invariant: Docker never prunes a
			// network that has a container attached, running or stopped.
			//
			// This runs for EVERY container, whatever the stored status says.
			// The invariant is about the network, not about one instance's
			// health, so an instance stuck mid-operation must not be skipped.
			if containerState != "not found" {
				connected, err := dockerClient.EnsureContainerOnNetwork(inst.ContainerID)
				if err != nil {
					log.Printf("Warning: reconcile: ensure oddk-bridge attachment of instance %s: %v", inst.Name, err)
				} else if connected {
					log.Printf("Warning: reconcile: container of instance %s was not attached to oddk-bridge (recreated outside ODDK?) - reconnected it", inst.Name)
				}
			}
		}

		newStatus, message := decideReconcile(inst.Status, inst.ContainerID, containerState)
		if message != "" {
			log.Printf("Reconcile: instance %s: %s", inst.Name, message)
		}
		if newStatus != "" && newStatus != inst.Status {
			reconcileSetStatus(st, inst.Name, newStatus)
		}
	}
}

// decideReconcile is the whole startup status matrix, as a pure function of the
// stored status and what Docker reports about the container. An empty
// newStatus means "leave the row alone"; message is what to tell the operator
// ("" = nothing worth saying).
//
// containerState is "" when the instance has no container ID, and otherwise one
// of the closed set GetContainerStatus returns: "running", "stopped", "paused",
// "restarting", "not found".
//
// It is a separate function purely so the matrix can be tested. reconcile takes
// a *docker.Client, which cannot be faked, so before this split the matrix had
// zero coverage — which is how it came to handle four pairs out of the ~forty
// that exist.
func decideReconcile(storedStatus instances.InstanceStatus, containerID, containerState string) (newStatus instances.InstanceStatus, message string) {
	// An interrupted status can only mean the operation that set it died
	// mid-flight: this pass runs before the executor accepts any work, so none
	// of them can be live.
	if remedy, interrupted := storedStatus.IsInterrupted(); interrupted {
		return instances.StatusError, fmt.Sprintf("stuck in %q (interrupted operation) - marking 'error'; %s", storedStatus, remedy)
	}

	// Anything unclassified is a status someone added without classifying it in
	// internal/store/instances/status.go. Fail loudly rather than leaving the row
	// alone — leaving it alone is exactly how "switching"/"upgrading"/
	// "reconfiguring" survived every restart forever.
	if !storedStatus.Valid() {
		return instances.StatusError, fmt.Sprintf("UNRECOGNISED status %q - marking 'error'. "+
			"If this is a new status, classify it in internal/store/instances/status.go", storedStatus)
	}

	if containerID == "" {
		if storedStatus != instances.StatusError {
			return instances.StatusError, fmt.Sprintf("status %q but no container ID - marking 'error'", storedStatus)
		}
		return "", ""
	}

	switch {
	case containerState == "not found":
		if storedStatus != instances.StatusError {
			return instances.StatusError, fmt.Sprintf("container (status %q) no longer exists - marking 'error'", storedStatus)
		}
		return "", ""

	case containerState == "paused" || containerState == "restarting":
		return "", fmt.Sprintf("container is %s (status %q) - leaving status unchanged", containerState, storedStatus)

	case storedStatus == instances.StatusError:
		// Never auto-cleared: by convention only an explicit operation promotes
		// out of 'error'. A container that happens to be up does not prove the
		// instance is well — 'restoring' becomes 'error' precisely because its
		// data may be incomplete while its server answers.
		return "", ""

	case containerState == "running" && storedStatus != instances.StatusRunning:
		return instances.StatusRunning, fmt.Sprintf("recorded %q but its container is running - marking 'running'", storedStatus)

	case containerState == "stopped" && storedStatus != instances.StatusStopped:
		return instances.StatusStopped, fmt.Sprintf("recorded %q but its container is stopped - marking 'stopped'; use 'instance start' to bring it back", storedStatus)

	case containerState == "running" || containerState == "stopped":
		return "", "" // store and Docker already agree

	default:
		return "", fmt.Sprintf("container reported an unexpected state %q (status %q) - leaving status unchanged", containerState, storedStatus)
	}
}

func reconcileSetStatus(st *store.Store, name string, status instances.InstanceStatus) {
	if err := st.Instances.UpdateStatus(name, status); err != nil {
		log.Printf("Error: reconcile: update status of instance %s to %q: %v", name, status, err)
	}
}

// Daemon-owned temp artifacts that operations stage inside the backup
// directory. Safe to delete at startup: operations are uninterruptible and
// none can be in flight before the HTTP server starts, so anything matching
// these prefixes was orphaned by a previous daemon run.
var staleBackupArtifactPrefixes = []string{
	".tmp-", ".pgpass-", ".restore-", ".upgrade-", operations.SnapshotStagingPrefix,
}

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

	// The managed downloads area holds archives fetched from S3 that no
	// catalogue row references; everything in it is re-fetchable, so aged
	// entries are pruned rather than warned about. (The scheduler re-runs this
	// daily for daemons that stay up for months.)
	if pruned, err := operations.SweepSnapshotDownloads(backupDir); err != nil {
		log.Printf("Warning: snapshot downloads sweep skipped: %v", err)
	} else if pruned > 0 {
		log.Printf("Pruned %d aged archive(s) from the snapshot downloads area (re-fetchable from S3)", pruned)
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

	// Snapshots have their OWN catalogue (snapshot_history), so a snapshot
	// archive is only an orphan if that catalogue does not reference it either.
	//
	// This used to blanket-skip every snapshot-prefixed file, because snapshots
	// were deliberately unrecorded and warning on each startup would have been
	// noise. They are recorded now, so the blanket skip would hide a genuinely
	// unmanaged archive — exactly what this check exists to surface.
	snapshotReferenced, err := st.Snapshot.ReferencedFilenames()
	if err != nil {
		log.Printf("Warning: startup snapshot record check skipped: %v", err)
		snapshotReferenced = map[string]bool{}
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".tar.zst") || referenced[name] {
			continue
		}
		if strings.HasPrefix(name, operations.SnapshotFilePrefix) {
			if snapshotReferenced[name] {
				continue
			}
			log.Printf("Warning: backup directory contains snapshot %s that no snapshot record references - ODDK will not manage, upload or clean it up", name)
			continue
		}
		log.Printf("Warning: backup directory contains archive %s that no backup record references - ODDK will not manage or clean it up", name)
	}
}

// reconcileInterruptedSnapshotRuns closes out scheduled-snapshot runs that a
// previous process started and never finished.
//
// Every incomplete row at startup is by definition from a dead process. Left
// alone it looks perpetually in-flight: the scheduler's dedup window counts it,
// so no further snapshot is attempted for the whole plan interval, while nothing
// records that the run failed. This is the audit signal — the same reasoning
// that converts a stuck instance status of "restoring" into "error".
//
// Deliberately scoped to the snapshot sentinel. Per-instance backup runs use a
// fixed one-hour dedup window and were not written with this in mind; widening
// it is a separate change.
func reconcileInterruptedSnapshotRuns(st *store.Store) {
	n, err := st.Cron.MarkInterruptedRuns(operations.SnapshotCronInstance)
	if err != nil {
		log.Printf("Warning: could not reconcile interrupted snapshot runs: %v", err)
		return
	}
	if n > 0 {
		log.Printf("Marked %d interrupted snapshot run(s) from a previous daemon process; the next scheduled slot will run normally", n)
	}
}
