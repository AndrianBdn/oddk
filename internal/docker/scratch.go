package docker

import (
	"fmt"
	"log"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/andrianbdn/oddk/internal/store/parameters"
)

// scratchVolumeLabel marks a scratch cluster's data volume. It is on the
// VOLUME, not only the container, because Docker can remove a container and
// fail to remove its volume while still reporting success (it only logs the
// volume error). A volume that outlives its container holds a full copy of an
// instance's cluster, and only a label on the volume itself lets the startup
// sweep find it.
const scratchVolumeLabel = "oddk.scratch-volume"

// ScratchContainerName names the throwaway cluster a single-database restore
// out of a physical snapshot is read from. Deliberately NOT ContainerName's
// "oddk-pg-" prefix: that name means "an instance's container", and a scratch
// copy of a cluster must never be mistaken for the instance it was copied from.
func ScratchContainerName(instanceName string, nonce int64) string {
	return fmt.Sprintf("oddk-scratch-%s-%d", instanceName, nonce)
}

// scratchOverrides are appended after the instance's own parameters, so they
// win over the parameter group (PostgreSQL applies the last -c for a name).
// Each one stops the scratch copy from acting as the instance it was copied
// from would — which matters even with no network, because a scheduled job
// that deletes local rows only needs the copy itself:
//   - max_worker_processes=0: no background workers at all, so no pg_cron
//     launcher, TimescaleDB job scheduler or pg_partman worker ever starts.
//     This is the control that matters: measured, a preloaded pg_cron with a
//     1-second DELETE job emptied the copy before pg_dump ran (0 of 100 rows
//     restored). A preloaded library whose worker cannot register only logs;
//     the server still starts. pg_dump's -j uses connections, not workers.
//   - default_transaction_read_only=on: a second layer against anything that
//     writes without a background worker. pg_dump is read-only anyway.
//     Per-role/per-database settings outrank -c, so this is defence in depth,
//     not the guarantee; max_worker_processes is.
//   - cron.launch_active_jobs=off: pg_cron's own switch (>= 1.5), belt and
//     braces. A dotted name is a harmless placeholder where pg_cron is absent.
//   - max_logical_replication_workers=0: a restored SUBSCRIPTION would
//     otherwise start consuming from its publisher and advance the real
//     subscriber's slot, losing its changes.
//   - archive_mode=off: an archive_command would otherwise ship this copy's WAL
//     into the real instance's archive.
//
// The container also has no network at all (see CreateScratchCluster), which
// is what stops dblink, postgres_fdw and outbound job payloads from reaching
// anything.
var scratchOverrides = []string{
	"max_worker_processes=0",
	"default_transaction_read_only=on",
	"cron.launch_active_jobs=off",
	"max_logical_replication_workers=0",
	"archive_mode=off",
}

// buildScratchContainer is buildInstanceContainer's spec reshaped for a
// throwaway, isolated copy of a cluster. The instance's parameter group and
// resources are kept on purpose: pg_dump takes a lock on every table of the
// database in one transaction, so a database that needs a raised
// max_locks_per_transaction on the instance needs it on the copy too.
func buildScratchContainer(instanceName, volumeName, version, imageName string, cpuCores, ramMB int, resolvedParams []parameters.ResolvedParameter) instanceContainerSpec {
	spec := buildInstanceContainer(instanceName, version, imageName, 0, cpuCores, ramMB, "", "", resolvedParams)

	for _, o := range scratchOverrides {
		spec.config.Cmd = append(spec.config.Cmd, "-c", o)
	}
	spec.config.ExposedPorts = nil
	// Labelled as a helper so the daemon's startup sweep removes a scratch
	// cluster a crash left behind — together with its volume, since the sweep
	// removes anonymous volumes (RemoveHelperContainers).
	spec.config.Labels = map[string]string{
		"oddk.helper":       "true",
		"oddk.scratch-copy": instanceName,
	}

	spec.hostConfig.PortBindings = nil
	// NO network. Helpers reach the server by joining this container's network
	// namespace and connecting to 127.0.0.1, exactly as pg_basebackup capture
	// does. A copy of a production cluster with outbound access could run the
	// source's subscriptions, foreign-data-wrapper queries and scheduled jobs
	// against real systems.
	spec.hostConfig.NetworkMode = container.NetworkMode("none")
	spec.networking = nil
	// A scratch volume created and labelled by CreateScratchCluster — never
	// the instance's own named volume.
	spec.hostConfig.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: volumeName, Target: pgDataMountTarget(version)}}
	// A crash-looping copy must fail the restore, not keep restarting.
	spec.hostConfig.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyDisabled}
	return spec
}

// CreateScratchCluster creates — but does not start — an isolated container
// that a physical snapshot entry can be streamed into and then read from, on
// its own labelled volume (named like the container). The caller owns removal
// (RemoveScratchCluster); the startup sweep is the backstop for both.
func (c *Client) CreateScratchCluster(containerName, instanceName, version, image string, cpuCores, ramMB int, parameterGroupParams []parameters.Parameter) (string, error) {
	if _, exists := c.CheckImageExists(image); !exists {
		return "", fmt.Errorf("image %s not found locally", image)
	}
	resolvedParams, err := parameters.ResolveParameters(parameterGroupParams, cpuCores, ramMB)
	if err != nil {
		return "", fmt.Errorf("resolve parameter group parameters: %w", err)
	}

	if _, err := c.cli.VolumeCreate(c.ctx, client.VolumeCreateOptions{
		Name:   containerName,
		Labels: map[string]string{scratchVolumeLabel: instanceName},
	}); err != nil {
		return "", fmt.Errorf("create scratch volume: %w", err)
	}

	spec := buildScratchContainer(instanceName, containerName, version, image, cpuCores, ramMB, resolvedParams)
	resp, err := c.cli.ContainerCreate(c.ctx, client.ContainerCreateOptions{
		Config:     spec.config,
		HostConfig: spec.hostConfig,
		Name:       containerName,
	})
	if err != nil {
		if _, rmErr := c.cli.VolumeRemove(c.ctx, containerName, client.VolumeRemoveOptions{Force: true}); rmErr != nil {
			log.Printf("Warning: could not remove scratch volume %s after a failed container create: %v", containerName, rmErr)
		}
		return "", fmt.Errorf("create scratch container: %w", err)
	}
	return resp.ID, nil
}

// RemoveScratchCluster force-removes a scratch container and then, separately,
// its volume — which holds a full copy of the instance's cluster. The volume
// removal is its own checked call on purpose: a container removed with
// RemoveVolumes reports success even when Docker failed to delete the volume,
// and the caller must be able to tell the operator a copy of their data is
// still on disk. An already-absent container or volume is not an error.
func (c *Client) RemoveScratchCluster(containerID, volumeName string) error {
	if _, err := c.cli.ContainerRemove(c.ctx, containerID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove scratch container: %w", err)
	}
	if _, err := c.cli.VolumeRemove(c.ctx, volumeName, client.VolumeRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove scratch volume %s: %w", volumeName, err)
	}
	return nil
}

// RemoveOrphanedScratchVolumes removes every labelled scratch volume. Run at
// daemon startup, AFTER RemoveHelperContainers: at that point no operation is
// running, so any scratch volume left is an orphan — from a crash, or from a
// removal Docker reported as successful but did not complete. Returns how many
// were removed.
func (c *Client) RemoveOrphanedScratchVolumes() (int, error) {
	list, err := c.cli.VolumeList(c.ctx, client.VolumeListOptions{
		Filters: make(client.Filters).Add("label", scratchVolumeLabel),
	})
	if err != nil {
		return 0, fmt.Errorf("list scratch volumes: %w", err)
	}
	removed := 0
	var failed []string
	for _, v := range list.Items {
		if _, err := c.cli.VolumeRemove(c.ctx, v.Name, client.VolumeRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			failed = append(failed, fmt.Sprintf("%s (%v)", v.Name, err))
			continue
		}
		removed++
	}
	if len(failed) > 0 {
		return removed, fmt.Errorf("could not remove scratch volume(s), each holding a copy of an instance's data: %s", strings.Join(failed, "; "))
	}
	return removed, nil
}
