package operations

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store/instances"
	"github.com/andrianbdn/oddk/internal/store/parameters"
)

// instanceRebuild is everything rebuildInstanceCluster needs to build one
// instance's cluster from its extracted snapshot entry. It is the shared core
// of `snapshot apply` and `snapshot restore-instance`: both create the volume
// and container, restore the data in the entry's format, and prove the result
// by waiting for readiness. Everything that can be refused has been refused by
// the time this runs, and the caller owns the row's status on failure — both
// already mark "error" in a defer.
type instanceRebuild struct {
	meta       *InstanceMeta
	entry      SnapshotInstanceEntry
	parameters []parameters.Parameter // the group the container is built with

	// password is the SOURCE's plaintext, the only credential the restored
	// cluster accepts: a logical restore replays globals.sql, which sets the
	// postgres role to the source's hash, and a physical one arrives with that
	// hash inside pg_authid. It is what the master key exists to recover.
	password string

	instanceDir string // the extracted instances/<name> directory

	// logical carries what a logical entry's dumps declare; nil for a physical
	// entry. Read BEFORE anything destructive (see readLogicalRestoreInputs).
	logical *logicalRestoreInputs

	progress io.Writer
}

// logicalRestoreInputs is what a logical entry's archive members declare:
// the databases to recreate (databases.json) and the roles globals.sql will
// create. Callers read them before their destructive phase, so a truncated
// archive is a refusal while refusing is still free — apply used to discover
// a missing databases.json only after the master key had been replaced.
type logicalRestoreInputs struct {
	databases []DatabaseMeta
	roleNames []string
}

// readLogicalRestoreInputs loads a logical entry's restore inputs from its
// extracted directory.
func readLogicalRestoreInputs(instanceDir, instanceName string) (*logicalRestoreInputs, error) {
	dbs, found, err := readDatabaseMetadata(instanceDir)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, operr.Invalidf("archive has no %s for instance %s", databaseMetadataFile, instanceName)
	}
	roleNames, err := roleNamesFromGlobals(filepath.Join(instanceDir, "globals.sql"))
	if err != nil {
		return nil, err
	}
	return &logicalRestoreInputs{databases: dbs, roleNames: roleNames}, nil
}

// rebuildResult reports what rebuildInstanceCluster produced.
type rebuildResult struct {
	databases   int
	finalStatus instances.InstanceStatus
}

// rebuildInstanceCluster creates the instance's volume and container, restores
// its data — streaming the archived cluster into the volume for a physical
// entry, replaying dumps for a logical one — and leaves the row at "running",
// or "stopped" for a cold-captured physical entry.
//
// Neither format gets the instance's real password: whatever is handed to the
// entrypoint stays in Docker's container config for the container's lifetime
// (see docker.containerEnv).
//   - physical: the volume is filled BEFORE the first start, so the entrypoint
//     skips initdb and reads nothing; the restored cluster brings the source's
//     own postgres role with it.
//   - logical: initdb runs, so it needs a password: it gets a throwaway, which
//     the ALTER after readiness replaces with the real one (and globals.sql
//     then re-asserts the same value from the source's hash).
func rebuildInstanceCluster(ctx context.Context, deps *Dependencies, rb instanceRebuild) (rebuildResult, error) {
	meta := rb.meta
	physical := entryFormat(rb.entry) == SnapshotFormatPhysical
	if !physical && rb.logical == nil {
		return rebuildResult{}, fmt.Errorf("instance %s: logical entry has no restore inputs (caller must read them before the destructive phase)", meta.Name)
	}

	initdbPassword := ""
	if !physical {
		initdbPassword = newInitdbCredential()
	}

	containerID, err := deps.Docker.CreateContainer(
		meta.Name, meta.Version, meta.Image, meta.Port, initdbPassword,
		meta.CPUCores, meta.RAMMB, meta.ParameterGroup, rb.parameters,
	)
	if err != nil {
		return rebuildResult{}, fmt.Errorf("create container: %w", err)
	}
	if err := deps.Store.Instances.UpdateContainerID(meta.Name, containerID); err != nil {
		return rebuildResult{}, fmt.Errorf("record container id: %w", err)
	}

	if physical {
		// The volume must be filled BEFORE the container's first start: the
		// entrypoint sees PG_VERSION and skips initdb, so the archived cluster
		// — roles, per-database GUCs, ACLs, collation state and all — comes up
		// as-is, recovered to consistency by PostgreSQL itself.
		if err := restorePhysicalIntoCreatedContainer(ctx, deps, containerID, meta.Version, rb.instanceDir); err != nil {
			return rebuildResult{}, err
		}
		emitLine(rb.progress, "  ✓ Volume created and cluster files restored")
		if err := deps.Docker.StartContainer(containerID); err != nil {
			return rebuildResult{}, fmt.Errorf("start container: %w", err)
		}
		if err := waitForPostgresReady(ctx, meta.Port, rb.password); err != nil {
			return rebuildResult{}, fmt.Errorf("restored cluster did not become ready: %w", err)
		}
		count, countErr := countUserDatabases(ctx, meta.Port, rb.password)
		if countErr != nil {
			return rebuildResult{}, fmt.Errorf("verify restored cluster: %w", countErr)
		}
		emitLine(rb.progress, "  ✓ PostgreSQL recovered; serving %d database(s)", count)

		if rb.entry.CaptureMode == captureModeCold {
			// The instance was STOPPED when captured; reproduce that. It was
			// still started once above — deliberately, because a restore whose
			// cluster has never reached readiness is not a verified restore.
			if err := deps.Docker.StopContainer(containerID); err != nil {
				return rebuildResult{}, fmt.Errorf("stop cold-captured instance after verification: %w", err)
			}
			if err := deps.Store.Instances.UpdateStatus(meta.Name, instances.StatusStopped); err != nil {
				return rebuildResult{}, fmt.Errorf("mark instance stopped: %w", err)
			}
			emitLine(rb.progress, "  ✓ Instance left stopped, matching its state when the snapshot was taken")
			return rebuildResult{databases: count, finalStatus: instances.StatusStopped}, nil
		}
		if err := deps.Store.Instances.UpdateStatus(meta.Name, instances.StatusRunning); err != nil {
			return rebuildResult{}, fmt.Errorf("mark instance running: %w", err)
		}
		return rebuildResult{databases: count, finalStatus: instances.StatusRunning}, nil
	}

	if err := deps.Docker.StartContainer(containerID); err != nil {
		return rebuildResult{}, fmt.Errorf("start container: %w", err)
	}
	emitLine(rb.progress, "  ✓ Volume and container created")

	if err := waitForPostgresReady(ctx, meta.Port, initdbPassword); err != nil {
		return rebuildResult{}, fmt.Errorf("cluster did not become ready: %w", err)
	}
	if err := adoptPostgresPassword(ctx, meta.Port, initdbPassword, rb.password); err != nil {
		return rebuildResult{}, fmt.Errorf("set the instance password: %w", err)
	}
	emitLine(rb.progress, "  ✓ PostgreSQL ready")

	restored, err := RestoreClusterFromArchive(ctx, deps, RestoreClusterParams{
		InstanceName:  meta.Name,
		Image:         meta.Image,
		Port:          meta.Port,
		Password:      rb.password,
		CPUCores:      meta.CPUCores,
		ExtractedDir:  rb.instanceDir,
		Databases:     rb.logical.databases,
		ExpectedRoles: rb.logical.roleNames,
	})
	if err != nil {
		return rebuildResult{}, err
	}
	emitLine(rb.progress, "  ✓ Roles and %d database(s) restored", restored)

	if err := deps.Store.Instances.UpdateStatus(meta.Name, instances.StatusRunning); err != nil {
		return rebuildResult{}, fmt.Errorf("mark instance running: %w", err)
	}
	return rebuildResult{databases: restored, finalStatus: instances.StatusRunning}, nil
}
