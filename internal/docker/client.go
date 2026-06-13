package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"

	"github.com/hypersequent/oddk/internal/store/parameters"
)

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
		if net.Name == "oddk-bridge" {
			log.Println("Network oddk-bridge already exists")
			return nil
		}
	}

	_, err = c.cli.NetworkCreate(c.ctx, "oddk-bridge", network.CreateOptions{
		Driver: "bridge",
		IPAM: &network.IPAM{
			Config: []network.IPAMConfig{
				{
					Subnet:  "10.88.0.0/16",
					Gateway: "10.88.0.1",
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

func pgDataMountTarget(version string) string {
	major, err := strconv.Atoi(strings.Split(version, ".")[0])
	if err == nil && major >= 18 {
		return "/var/lib/postgresql"
	}
	return "/var/lib/postgresql/data"
}

func (c *Client) CreateContainer(name, version, image string, port int, password string, cpuCores, ramMB int, parameterGroupName string, parameterGroupParams []parameters.Parameter) (string, error) {
	volumeName := fmt.Sprintf("oddk-data-%s", name)
	containerName := fmt.Sprintf("oddk-pg-%s", name)
	imageName := image

	// Verify the image exists locally
	tags, exists := c.CheckImageExists(imageName)
	if !exists {
		return "", fmt.Errorf("image %s not found locally. Please run 'oddk pull --image %s' first", imageName, imageName)
	}
	log.Printf("Using existing image %s (tags: %v)", imageName, tags)

	// Check if volume already exists - we don't support adopting existing volumes
	_, err := c.cli.VolumeInspect(c.ctx, volumeName)
	if err == nil {
		return "", fmt.Errorf("volume %s already exists. ODDK does not support adopting existing volumes. Please remove the existing volume or use a different instance name", volumeName)
	}

	_, err = c.cli.VolumeCreate(c.ctx, volume.CreateOptions{
		Name: volumeName,
	})
	if err != nil {
		return "", fmt.Errorf("create volume: %w", err)
	}

	// Resolve parameter group parameters
	resolvedParams, err := parameters.ResolveParameters(parameterGroupParams, cpuCores, ramMB)
	if err != nil {
		_ = c.cli.VolumeRemove(c.ctx, volumeName, true)
		return "", fmt.Errorf("resolve parameter group parameters: %w", err)
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
				HostIP:   "10.88.0.1",
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
		Image: imageName,
		Env: []string{
			fmt.Sprintf("POSTGRES_PASSWORD=%s", password),
			"POSTGRES_USER=postgres",
		},
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
			"oddk-bridge": {},
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

// RecreateContainer stops and removes the old container, then creates a new one with the same volume but new parameters
func (c *Client) RecreateContainer(name, version, image string, port int, password string, cpuCores, ramMB int, parameterGroupName string, parameterGroupParams []parameters.Parameter, oldContainerID string) (string, error) {
	volumeName := fmt.Sprintf("oddk-data-%s", name)
	containerName := fmt.Sprintf("oddk-pg-%s", name)
	imageName := image

	// Verify the image exists locally
	tags, exists := c.CheckImageExists(imageName)
	if !exists {
		return "", fmt.Errorf("image %s not found locally. Please run 'oddk pull --image %s' first", imageName, imageName)
	}
	log.Printf("Using existing image %s (tags: %v)", imageName, tags)

	// Stop the old container if running
	if oldContainerID != "" {
		status, err := c.GetContainerStatus(oldContainerID)
		if err != nil {
			log.Printf("Warning: could not get container status: %v", err)
		} else if status == "running" {
			if err := c.StopContainer(oldContainerID); err != nil {
				return "", fmt.Errorf("stop old container: %w", err)
			}
		}

		if err := c.RemoveContainer(oldContainerID); err != nil {
			return "", fmt.Errorf("remove old container: %w", err)
		}
	}

	// Verify the volume exists (it should, since we're reconfiguring)
	_, err := c.cli.VolumeInspect(c.ctx, volumeName)
	if err != nil {
		return "", fmt.Errorf("volume %s not found: %w", volumeName, err)
	}

	// Resolve parameter group parameters
	resolvedParams, err := parameters.ResolveParameters(parameterGroupParams, cpuCores, ramMB)
	if err != nil {
		return "", fmt.Errorf("resolve parameter group parameters: %w", err)
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
				HostIP:   "10.88.0.1",
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
		Image: imageName,
		Env: []string{
			fmt.Sprintf("POSTGRES_PASSWORD=%s", password),
			"POSTGRES_USER=postgres",
		},
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
			"oddk-bridge": {},
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

// PullImage pulls a Docker image from the registry
func (c *Client) PullImage(imageName string) error {
	log.Printf("Pulling image %s...", imageName)
	reader, err := c.cli.ImagePull(c.ctx, imageName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image: %w", err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			log.Printf("Error closing image pull reader: %v", err)
		}
	}()

	// Read the output to completion
	_, _ = io.Copy(io.Discard, reader)

	log.Printf("Successfully pulled image %s", imageName)
	return nil
}

func (c *Client) GetContainerStatus(containerID string) (string, error) {
	inspect, err := c.cli.ContainerInspect(c.ctx, containerID)
	if err != nil {
		if strings.Contains(err.Error(), "No such container") {
			return "not found", nil
		}
		return "", fmt.Errorf("inspect container: %w", err)
	}

	if inspect.State.Running {
		return "running", nil
	}
	if inspect.State.Paused {
		return "paused", nil
	}
	if inspect.State.Restarting {
		return "restarting", nil
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
