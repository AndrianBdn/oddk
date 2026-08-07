package operations

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/andrianbdn/oddk/internal/util"
)

// helperContainerSpec describes one ephemeral helper container run. Every
// helper shares the same lifecycle contract: labeled oddk.helper=true (so the
// daemon-startup sweep can reap orphans after a crash), run as the daemon's
// uid/gid (so files it stages are readable and deletable without a chown pass),
// given the postgres password via a short-lived read-only .pgpass mount (never
// an env var, so it is not exposed via docker inspect), attached to oddk-bridge
// unless it joins an instance's network namespace, and force-removed with a
// detached context so a cancelled op ctx doesn't leave it running.
type helperContainerSpec struct {
	ContainerName string
	Image         string
	Cmd           []string
	// Password is written to the .pgpass mount; PGPASSFILE points at it.
	Password string
	// Mounts are added in addition to the .pgpass mount.
	Mounts []mount.Mount
	// JoinNetNSContainer, when set, joins that container's network namespace
	// (NetworkMode container:<id>) instead of attaching to oddk-bridge — a
	// container sharing another's netns cannot also be attached to networks.
	JoinNetNSContainer string
	// Tool names the client binary in the non-zero-exit error, e.g.
	// "pg_dump failed with status 1: ...". Empty means "helper".
	Tool string
	// LogOutputOnSuccess logs the helper's output even on exit 0 — used for
	// the globals psql run, which exits 0 under ON_ERROR_STOP=0 even when
	// individual statements error, so the detail is preserved for diagnosis.
	LogOutputOnSuccess bool
	// CaptureStdout, when non-nil, receives the helper's stdout after a
	// successful run — used for pg_dumpall, which writes SQL to stdout.
	CaptureStdout io.Writer
	// FailureHint, when non-nil, is given the failed helper's logs and returns
	// extra context appended to the error (may be empty).
	FailureHint func(logs string) string
}

// runHelperContainer creates, starts, and waits on an ephemeral helper
// container per spec. It returns an error if the container exits non-zero,
// including its logs.
func runHelperContainer(ctx context.Context, deps *Dependencies, spec helperContainerSpec) error {
	tool := spec.Tool
	if tool == "" {
		tool = "helper"
	}

	pgPassMount, pgPassEnv, cleanup, err := newPgPassMount(deps.BackupDir, spec.Password)
	if err != nil {
		return err
	}
	defer cleanup()

	config := &container.Config{
		Image:  spec.Image,
		User:   fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		Cmd:    spec.Cmd,
		Env:    []string{pgPassEnv},
		Labels: map[string]string{"oddk.helper": "true"},
	}
	hostConfig := &container.HostConfig{Mounts: append(append([]mount.Mount{}, spec.Mounts...), pgPassMount)}
	var networkConfig *network.NetworkingConfig
	if spec.JoinNetNSContainer != "" {
		hostConfig.NetworkMode = container.NetworkMode("container:" + spec.JoinNetNSContainer)
	} else {
		networkConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				util.OddkNetworkName: {},
			},
		}
	}

	cli := deps.Docker.GetDockerClient()
	resp, err := cli.ContainerCreate(ctx, config, hostConfig, networkConfig, nil, spec.ContainerName)
	if err != nil {
		return fmt.Errorf("create %s container: %w", tool, err)
	}
	defer func() {
		// Detached context so a cancelled op ctx doesn't leave the helper
		// running; the daemon-startup sweep is the backstop.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = cli.ContainerRemove(cleanupCtx, resp.ID, container.RemoveOptions{Force: true})
	}()

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start %s container: %w", tool, err)
	}

	statusCh, errCh := cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("wait for %s container: %w", tool, err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			logs, logErr := getContainerLogs(ctx, deps, resp.ID)
			if logErr != nil {
				logs = fmt.Sprintf("<logs unavailable: %v>", logErr)
			}
			hint := ""
			if spec.FailureHint != nil {
				hint = spec.FailureHint(logs)
			}
			return fmt.Errorf("%s failed with status %d: %s%s", tool, status.StatusCode, logs, hint)
		}
	}

	if spec.CaptureStdout != nil {
		reader, err := cli.ContainerLogs(ctx, resp.ID, container.LogsOptions{
			ShowStdout: true,
			ShowStderr: false,
		})
		if err != nil {
			return fmt.Errorf("get container logs: %w", err)
		}
		defer func() { _ = reader.Close() }()
		if _, err := stdcopy.StdCopy(spec.CaptureStdout, io.Discard, reader); err != nil {
			return fmt.Errorf("read container output: %w", err)
		}
	}

	if spec.LogOutputOnSuccess {
		if logs, lerr := getContainerLogs(ctx, deps, resp.ID); lerr == nil && strings.TrimSpace(logs) != "" {
			log.Printf("[%s] output:\n%s", spec.ContainerName, logs)
		}
	}
	return nil
}

// getContainerLogs fetches a container's combined stdout/stderr, demultiplexed.
func getContainerLogs(ctx context.Context, deps *Dependencies, containerID string) (string, error) {
	reader, err := deps.Docker.GetDockerClient().ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = reader.Close() }()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, reader); err != nil {
		return "", err
	}

	return fmt.Sprintf("stdout: %s\nstderr: %s", stdout.String(), stderr.String()), nil
}
