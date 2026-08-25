package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"

	"github.com/andrianbdn/oddk/internal/store/parameters"
	"github.com/andrianbdn/oddk/internal/util"
)

// containerEnv builds an instance container's environment.
//
// POSTGRES_PASSWORD is included ONLY when the container's first start will run
// initdb, which is the only time the entrypoint reads it: with a populated data
// directory it prints "Skipping initialization" and discards the value
// (verified against the postgres image's docker-entrypoint.sh, which also
// `unset "${!POSTGRES_@}"`s before exec'ing postgres). Docker keeps Config.Env
// for the container's whole lifetime and hands it to anything that can read
// container metadata, so passing a credential the entrypoint cannot use is
// exposure with no function.
//
// Omitting it also converts the one case that would otherwise be silent into a
// loud one: if a recreate ever met an unexpectedly EMPTY volume, the entrypoint
// now refuses to start ("You must specify POSTGRES_PASSWORD") instead of
// running initdb and producing a healthy-looking empty cluster where the
// instance's data should have been.
//
// This is not a confidentiality boundary against an attacker — anyone who can
// read Config.Env can also `docker exec ... psql -U postgres`, which the data
// directory's default `local all all trust` line answers without any password
// at all. It removes a live credential from metadata that gets copied around
// (support bundles, observability agents holding a read-only docker.sock).
func containerEnv(password string) []string {
	env := []string{"POSTGRES_USER=postgres"}
	if password != "" {
		env = append(env, fmt.Sprintf("POSTGRES_PASSWORD=%s", password))
	}
	return env
}

// ErrPreflight marks a RecreateContainer/CreateContainer failure that happened
// before any existing container was removed. Callers must not roll back on it
// — the previous container is still the one that is running.
var ErrPreflight = errors.New("container not modified")

func preflightErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrPreflight}, args...)...)
}

// ContainerName and VolumeName are the only places these two names are spelled.
// They are exported because the operations layer removes both by name during
// destroy — a container whose ID the store never learned still has to go, and a
// destroy that leaves the volume behind makes `oddk create --name <same>` fail
// forever (CreateContainer refuses to adopt an existing volume).
func ContainerName(instanceName string) string {
	return fmt.Sprintf("oddk-pg-%s", instanceName)
}

func VolumeName(instanceName string) string {
	return fmt.Sprintf("oddk-data-%s", instanceName)
}

type Client struct {
	cli *client.Client
	ctx context.Context
}

func NewClient() (*Client, error) {
	socketPaths := []string{
		"/var/run/docker.sock",
		"/var/lib/docker.sock",
	}

	if home := os.Getenv("HOME"); home != "" {
		socketPaths = append(socketPaths, filepath.Join(home, ".docker/run/docker.sock"))
	}

	var dockerSocket string
	for _, path := range socketPaths {
		//nolint:gosec // G703: socketPaths is a fixed allowlist; $HOME is the operator's own trusted env, not attacker input
		if _, err := os.Stat(path); err == nil {
			dockerSocket = path
			break
		}
	}

	if dockerSocket == "" {
		return nil, fmt.Errorf("docker socket not found")
	}

	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+dockerSocket),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	ctx := context.Background()

	if _, err := cli.Ping(ctx); err != nil {
		return nil, fmt.Errorf("docker daemon not responding: %w", err)
	}

	c := &Client{
		cli: cli,
		ctx: ctx,
	}

	if err := c.ensureNetwork(); err != nil {
		return nil, fmt.Errorf("ensure network: %w", err)
	}

	return c, nil
}

func (c *Client) ensureNetwork() error {
	networks, err := c.cli.NetworkList(c.ctx, network.ListOptions{})
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}

	for _, net := range networks {
		if net.Name == util.OddkNetworkName {
			log.Println("Network oddk-bridge already exists")
			return nil
		}
	}

	_, err = c.cli.NetworkCreate(c.ctx, util.OddkNetworkName, network.CreateOptions{
		Driver: "bridge",
		IPAM: &network.IPAM{
			Config: []network.IPAMConfig{
				{
					Subnet:  util.OddkSubnet,
					Gateway: util.GatewayIP,
				},
			},
		},
		Options: map[string]string{
			"com.docker.network.bridge.name": "oddk0",
		},
	})
	if err != nil {
		return fmt.Errorf("create network: %w", err)
	}

	log.Println("Created network oddk-bridge with subnet 10.88.0.0/16")
	return nil
}

// EnsureContainerOnNetwork checks that the container is attached to the
// oddk-bridge network and connects it if not. Docker refuses to remove or
// prune a network that has containers attached (running or stopped), so
// keeping every instance container attached is what protects oddk-bridge
// from 'docker network prune'. Returns true if a connect was performed.
func (c *Client) EnsureContainerOnNetwork(containerID string) (bool, error) {
	inspect, err := c.cli.ContainerInspect(c.ctx, containerID)
	if err != nil {
		return false, fmt.Errorf("inspect container: %w", err)
	}

	if inspect.NetworkSettings != nil {
		if _, attached := inspect.NetworkSettings.Networks[util.OddkNetworkName]; attached {
			return false, nil
		}
	}

	if err := c.cli.NetworkConnect(c.ctx, util.OddkNetworkName, containerID, nil); err != nil {
		return false, fmt.Errorf("connect container to oddk-bridge: %w", err)
	}
	return true, nil
}

func pgDataMountTarget(version string) string {
	major, err := strconv.Atoi(strings.Split(version, ".")[0])
	if err == nil && major >= 18 {
		return "/var/lib/postgresql"
	}
	return "/var/lib/postgresql/data"
}

// PGDataMountTarget is where an instance's data volume is mounted inside its
// container: the data directory itself for PG <= 17, its parent for PG 18+
// (the official image moved PGDATA to /var/lib/postgresql/<major>/docker).
// Exported for the physical snapshot paths, which must know where inside the
// volume the cluster lives.
func PGDataMountTarget(version string) string {
	return pgDataMountTarget(version)
}

// ImagePGMajor reports the PostgreSQL major version a local image serves, read
// from the PG_MAJOR environment variable the official images (and everything
// derived from them: pgvector, postgis, ...) carry. Best-effort: ok is false
// when the image cannot be inspected or does not declare PG_MAJOR, and callers
// must skip their check rather than refuse — a non-standard image is not proof
// of a mismatch.
func (c *Client) ImagePGMajor(imageName string) (int, bool) {
	inspect, err := c.cli.ImageInspect(c.ctx, imageName)
	if err != nil || inspect.Config == nil {
		return 0, false
	}
	return ParsePGMajorEnv(inspect.Config.Env)
}

// ParsePGMajorEnv extracts PG_MAJOR from a docker env list. Exported for the
// package's external tests, matching its other pure helpers.
func ParsePGMajorEnv(env []string) (int, bool) {
	for _, e := range env {
		if value, ok := strings.CutPrefix(e, "PG_MAJOR="); ok {
			// Historically "9.6"-style values existed; the major is the part
			// before the first dot either way.
			major, err := strconv.Atoi(strings.Split(value, ".")[0])
			if err == nil && major > 0 {
				return major, true
			}
			return 0, false
		}
	}
	return 0, false
}

// ContainerPGData returns the container's effective PGDATA path, read from its
// environment (which includes image-inherited variables). The fallback mirrors
// the official images' conventions for containers that somehow lack the
// variable: the volume mount target for PG <= 17, <target>/<major>/docker for
// PG 18+.
func (c *Client) ContainerPGData(containerID, version string) (string, error) {
	inspect, err := c.cli.ContainerInspect(c.ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}
	if inspect.Config != nil {
		for _, env := range inspect.Config.Env {
			if value, ok := strings.CutPrefix(env, "PGDATA="); ok && value != "" {
				return value, nil
			}
		}
	}
	target := pgDataMountTarget(version)
	if major, err := strconv.Atoi(strings.Split(version, ".")[0]); err == nil && major >= 18 {
		return fmt.Sprintf("%s/%d/docker", target, major), nil
	}
	return target, nil
}

func (c *Client) CreateContainer(name, version, image string, port int, password string, cpuCores, ramMB int, parameterGroupName string, parameterGroupParams []parameters.Parameter) (string, error) {
	volumeName := VolumeName(name)
	containerName := ContainerName(name)
	imageName := image

	// Verify the image exists locally
	tags, exists := c.CheckImageExists(imageName)
	if !exists {
		return "", preflightErrorf("image %s not found locally. Please run 'oddk pull --image %s' first", imageName, imageName)
	}
	log.Printf("Using existing image %s (tags: %v)", imageName, tags)

	// Resolve and refuse an oversized group BEFORE creating a volume we would
	// then have to roll back. PUT only type-checks against dummy 8 GiB, so a
	// group that is fine there can still be fatal for this instance's RAM.
	resolvedParams, err := parameters.ResolveParameters(parameterGroupParams, cpuCores, ramMB)
	if err != nil {
		return "", preflightErrorf("resolve parameter group parameters: %w", err)
	}
	if err := parameters.ValidateForContainer(resolvedParams, ramMB); err != nil {
		return "", preflightErrorf("%w", err)
	}

	// Check if volume already exists - we don't support adopting existing volumes
	_, err = c.cli.VolumeInspect(c.ctx, volumeName)
	if err == nil {
		return "", preflightErrorf("volume %s already exists. ODDK does not support adopting existing volumes. Please remove the existing volume or use a different instance name", volumeName)
	}

	_, err = c.cli.VolumeCreate(c.ctx, volume.CreateOptions{
		Name: volumeName,
	})
	if err != nil {
		return "", fmt.Errorf("create volume: %w", err)
	}

	// Convert RAM MB to bytes
	memLimit := int64(ramMB * 1024 * 1024)

	// Calculate CPU shares based on core count (1024 per core is Docker's default)
	cpuShares := int64(cpuCores * 1024)

	// Set shared memory size as a percentage of RAM (typically 25% of RAM for PostgreSQL)
	shmSize := memLimit / 4

	exposedPorts := nat.PortSet{
		"5432/tcp": struct{}{},
	}

	portBindings := nat.PortMap{
		"5432/tcp": []nat.PortBinding{
			{
				HostIP:   util.GatewayIP,
				HostPort: fmt.Sprintf("%d", port),
			},
		},
	}

	cmd := []string{"postgres"}
	for _, param := range resolvedParams {
		if param.Type == "postgres_cli_arg" {
			cmd = append(cmd, "-c", fmt.Sprintf("%s=%s", param.Name, param.Value))
		}
	}

	config := &container.Config{
		Image:        imageName,
		Env:          containerEnv(password),
		Cmd:          cmd,
		ExposedPorts: exposedPorts,
		Labels: map[string]string{
			"io.hpsq.oddk.instancename": name,
			"io.hpsq.oddk.pgroup":       parameterGroupName,
		},
	}

	hostConfig := &container.HostConfig{
		PortBindings: portBindings,
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: volumeName,
				Target: pgDataMountTarget(version),
			},
		},
		Resources: container.Resources{
			Memory:    memLimit,
			CPUShares: cpuShares,
		},
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
		ShmSize: shmSize,
		LogConfig: container.LogConfig{
			Type: "json-file",
			Config: map[string]string{
				"max-size": "5m",
				"max-file": "2",
			},
		},
	}

	networkConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			util.OddkNetworkName: {},
		},
	}

	resp, err := c.cli.ContainerCreate(c.ctx, config, hostConfig, networkConfig, nil, containerName)
	if err != nil {
		if rmErr := c.cli.VolumeRemove(c.ctx, volumeName, true); rmErr != nil {
			log.Printf("Error removing volume after container creation failure: %v", rmErr)
		}
		return "", fmt.Errorf("create container: %w", err)
	}

	return resp.ID, nil
}

func (c *Client) StartContainer(containerID string) error {
	if err := c.cli.ContainerStart(c.ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start container: %w", err)
	}

	// Readiness is the caller's responsibility: every start path now waits for
	// PostgreSQL to accept connections via waitForPostgresReady, so a blind
	// fixed sleep here would only add latency.
	return nil
}

// RecreateContainer stops and removes the old container, then creates a new one with the same volume but new parameters.
//
// Cheap checks (image, parameter resolution, shared-memory fit, volume exists)
// run BEFORE the old container is removed. Failures of those wrap ErrPreflight
// so the caller knows the previous container is still the running one.
//
// It deliberately takes NO password: the volume it reuses is populated by
// construction (preflight refuses a missing one), so the entrypoint skips
// initdb and would discard the value. Keeping the parameter off the signature
// is what stops it being reintroduced. See containerEnv.
func (c *Client) RecreateContainer(name, version, image string, port, cpuCores, ramMB int, parameterGroupName string, parameterGroupParams []parameters.Parameter, oldContainerID string) (string, error) {
	volumeName := VolumeName(name)
	containerName := ContainerName(name)
	imageName := image

	tags, exists := c.CheckImageExists(imageName)
	if !exists {
		return "", preflightErrorf("image %s not found locally. Please run 'oddk pull --image %s' first", imageName, imageName)
	}
	log.Printf("Using existing image %s (tags: %v)", imageName, tags)

	resolvedParams, err := parameters.ResolveParameters(parameterGroupParams, cpuCores, ramMB)
	if err != nil {
		return "", preflightErrorf("resolve parameter group parameters: %w", err)
	}
	if err := parameters.ValidateForContainer(resolvedParams, ramMB); err != nil {
		return "", preflightErrorf("%w", err)
	}

	if _, err := c.cli.VolumeInspect(c.ctx, volumeName); err != nil {
		return "", preflightErrorf("volume %s not found: %w", volumeName, err)
	}

	if oldContainerID != "" {
		status, statusErr := c.GetContainerStatus(oldContainerID)
		if statusErr != nil {
			log.Printf("Warning: could not get container status: %v", statusErr)
		} else if status == "running" {
			if err := c.StopContainer(oldContainerID); err != nil {
				return "", fmt.Errorf("stop old container: %w", err)
			}
		}
		if err := c.RemoveContainer(oldContainerID); err != nil {
			return "", fmt.Errorf("remove old container: %w", err)
		}
	}
	// A crash between ContainerCreate and UpdateContainerID leaves a container
	// named oddk-pg-<name> whose ID the store does not know. Removing by name
	// as well as ID makes a retry replace that orphan instead of hitting a
	// name conflict. No-op when the ID remove above already took it.
	if err := c.RemoveContainer(containerName); err != nil {
		return "", fmt.Errorf("remove leftover container %s: %w", containerName, err)
	}

	// Convert RAM MB to bytes
	memLimit := int64(ramMB * 1024 * 1024)

	// Calculate CPU shares based on core count (1024 per core is Docker's default)
	cpuShares := int64(cpuCores * 1024)

	// Set shared memory size as a percentage of RAM (typically 25% of RAM for PostgreSQL)
	shmSize := memLimit / 4

	exposedPorts := nat.PortSet{
		"5432/tcp": struct{}{},
	}

	portBindings := nat.PortMap{
		"5432/tcp": []nat.PortBinding{
			{
				HostIP:   util.GatewayIP,
				HostPort: fmt.Sprintf("%d", port),
			},
		},
	}

	cmd := []string{"postgres"}
	for _, param := range resolvedParams {
		if param.Type == "postgres_cli_arg" {
			cmd = append(cmd, "-c", fmt.Sprintf("%s=%s", param.Name, param.Value))
		}
	}

	config := &container.Config{
		Image:        imageName,
		Env:          containerEnv(""),
		Cmd:          cmd,
		ExposedPorts: exposedPorts,
		Labels: map[string]string{
			"io.hpsq.oddk.instancename": name,
			"io.hpsq.oddk.pgroup":       parameterGroupName,
		},
	}

	hostConfig := &container.HostConfig{
		PortBindings: portBindings,
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: volumeName,
				Target: pgDataMountTarget(version),
			},
		},
		Resources: container.Resources{
			Memory:    memLimit,
			CPUShares: cpuShares,
		},
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
		ShmSize: shmSize,
		LogConfig: container.LogConfig{
			Type: "json-file",
			Config: map[string]string{
				"max-size": "5m",
				"max-file": "2",
			},
		},
	}

	networkConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			util.OddkNetworkName: {},
		},
	}

	resp, err := c.cli.ContainerCreate(c.ctx, config, hostConfig, networkConfig, nil, containerName)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	return resp.ID, nil
}

func (c *Client) StopContainer(containerID string) error {
	timeout := 30
	if err := c.cli.ContainerStop(c.ctx, containerID, container.StopOptions{
		Timeout: &timeout,
	}); err != nil {
		return fmt.Errorf("stop container: %w", err)
	}
	return nil
}

func (c *Client) RemoveContainer(containerID string) error {
	if err := c.cli.ContainerRemove(c.ctx, containerID, container.RemoveOptions{
		Force: true,
	}); err != nil {
		if !strings.Contains(err.Error(), "No such container") {
			return fmt.Errorf("remove container: %w", err)
		}
	}
	return nil
}

// RemoveHelperContainers force-removes any containers labeled as ODDK helpers
// (label oddk.helper=true). Called at daemon startup to clean up orphans left
// by a previous crashed daemon run. Returns the number of containers removed.
//
// Single-host single-daemon deployment is assumed. If a second ODDK daemon is
// running on the same host, this will rip out its in-flight helper containers.
func (c *Client) RemoveHelperContainers() (int, error) {
	f := filters.NewArgs()
	f.Add("label", "oddk.helper=true")

	containers, err := c.cli.ContainerList(c.ctx, container.ListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		return 0, fmt.Errorf("list helper containers: %w", err)
	}

	removed := 0
	for _, ctr := range containers {
		if err := c.cli.ContainerRemove(c.ctx, ctr.ID, container.RemoveOptions{Force: true}); err != nil {
			if !strings.Contains(err.Error(), "No such container") {
				log.Printf("Warning: failed to remove orphaned helper %s: %v", ctr.ID[:12], err)
				continue
			}
		}
		removed++
	}
	return removed, nil
}

func (c *Client) RemoveVolume(volumeName string) error {
	if err := c.cli.VolumeRemove(c.ctx, volumeName, true); err != nil {
		if !strings.Contains(err.Error(), "no such volume") {
			return fmt.Errorf("remove volume: %w", err)
		}
	}
	return nil
}

// CheckImageExists checks if a Docker image exists locally and returns its tags
func (c *Client) CheckImageExists(imageName string) ([]string, bool) {
	images, err := c.cli.ImageList(c.ctx, image.ListOptions{})
	if err != nil {
		log.Printf("Error listing images: %v", err)
		return nil, false
	}

	var tags []string
	for _, img := range images {
		for _, tag := range img.RepoTags {
			if tag == imageName {
				tags = append(tags, tag)
			}
		}
	}

	if len(tags) > 0 {
		return tags, true
	}

	return nil, false
}

// GetImageID returns the local image ID (sha256:...) that an image tag/name
// currently resolves to, if the image is present locally. After re-pulling a
// moving tag (e.g. postgres:18) this reflects the newest patch, which can be
// compared against a container's image ID to detect a pending update.
func (c *Client) GetImageID(imageName string) (string, bool) {
	images, err := c.cli.ImageList(c.ctx, image.ListOptions{})
	if err != nil {
		log.Printf("Error listing images: %v", err)
		return "", false
	}
	for _, img := range images {
		if slices.Contains(img.RepoTags, imageName) {
			return img.ID, true
		}
	}
	return "", false
}

// GetContainerImageID returns the ID (sha256:...) of the image a container was
// created from. Comparable with GetImageID's result.
func (c *Client) GetContainerImageID(containerID string) (string, error) {
	inspect, err := c.cli.ContainerInspect(c.ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}
	return inspect.Image, nil
}

// PullImageProgress pulls a Docker image from the registry, streaming the raw
// Docker progress JSON (newline-delimited jsonmessage frames) to progress as it
// arrives so a caller can render live progress. progress may be nil.
//
// Unlike a bare io.Copy of the pull stream, this also decodes each frame to
// detect an embedded pull error (Docker reports failures such as "manifest
// unknown" inside the stream and still returns a clean EOF), so the returned
// error reflects the real outcome.
//
// If writing to progress fails (e.g. the client disconnected), the pull is NOT
// aborted: progress writes are dropped and the stream is drained to completion
// so the image still lands in the local cache. Operations are uninterruptible
// by client disconnect by design.
func (c *Client) PullImageProgress(ctx context.Context, imageName string, progress io.Writer) error {
	log.Printf("Pulling image %s...", imageName)
	reader, err := c.cli.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image: %w", err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			log.Printf("Error closing image pull reader: %v", err)
		}
	}()

	if progress == nil {
		progress = io.Discard
	}

	// Tee the raw frames to the client while decoding them here to surface any
	// in-stream error. The tolerant writer keeps the pull going if the client
	// goes away mid-stream.
	dec := json.NewDecoder(io.TeeReader(reader, &tolerantWriter{w: progress}))
	for {
		var msg jsonmessage.JSONMessage
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("read pull progress: %w", err)
		}
		if msg.Error != nil {
			return fmt.Errorf("pull image: %s", msg.Error.Message)
		}
	}

	log.Printf("Successfully pulled image %s", imageName)
	return nil
}

// tolerantWriter forwards writes to w until the first write error, after which
// it silently discards subsequent writes. It never returns an error, so a
// TeeReader feeding it keeps reading even after the destination (e.g. an HTTP
// client) goes away.
type tolerantWriter struct {
	w      io.Writer
	failed bool
}

func (t *tolerantWriter) Write(p []byte) (int, error) {
	if !t.failed {
		if _, err := t.w.Write(p); err != nil {
			t.failed = true
		}
	}
	return len(p), nil
}

func (c *Client) GetContainerStatus(containerID string) (string, error) {
	inspect, err := c.cli.ContainerInspect(c.ctx, containerID)
	if err != nil {
		if strings.Contains(err.Error(), "No such container") {
			return "not found", nil
		}
		return "", fmt.Errorf("inspect container: %w", err)
	}

	// ORDER MATTERS: Docker reports State.Running == true for a PAUSED container
	// (verified: a paused container inspects as Running=true, Paused=true,
	// Status="paused"). Testing Running first therefore made "paused"
	// unreachable and reported a frozen container as healthy — so callers that
	// branch on "paused" (snapshot capture, startup reconciliation) never saw
	// it, and a base backup against a paused server would hang rather than
	// degrade. Check the more specific states first.
	if inspect.State.Paused {
		return "paused", nil
	}
	if inspect.State.Restarting {
		return "restarting", nil
	}
	if inspect.State.Running {
		return "running", nil
	}

	return "stopped", nil
}

// GetContainerLogs returns the last N lines of logs from a container.
// Both stdout and stderr are merged into a single string.
func (c *Client) GetContainerLogs(containerID, tail string) (string, error) {
	reader, err := c.cli.ContainerLogs(c.ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
	})
	if err != nil {
		return "", fmt.Errorf("get container logs: %w", err)
	}
	defer func() { _ = reader.Close() }()

	var buf strings.Builder
	// stdcopy demultiplexes Docker's 8-byte framed stream (stdout + stderr merged)
	if _, err := stdcopy.StdCopy(&buf, &buf, reader); err != nil {
		return "", fmt.Errorf("read container logs: %w", err)
	}

	return buf.String(), nil
}

// StreamContainerLogs streams container logs to w until ctx is cancelled or the container stops.
func (c *Client) StreamContainerLogs(ctx context.Context, containerID, tail string, w io.Writer) error {
	reader, err := c.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       tail,
	})
	if err != nil {
		return fmt.Errorf("get container logs: %w", err)
	}
	defer func() { _ = reader.Close() }()

	if _, err := stdcopy.StdCopy(w, w, reader); err != nil && ctx.Err() == nil {
		return fmt.Errorf("stream container logs: %w", err)
	}
	return nil
}

// GetDockerClient returns the underlying Docker client for advanced operations
func (c *Client) GetDockerClient() *client.Client {
	return c.cli
}
