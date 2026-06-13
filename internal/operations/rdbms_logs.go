package operations

import (
	"context"
	"fmt"
	"io"

	"github.com/hypersequent/oddk/internal/operr"
)

// GetLogsParams contains parameters for getting instance logs
type GetLogsParams struct {
	InstanceName string
	Tail         string
}

// GetLogsResult contains the result of getting instance logs
type GetLogsResult struct {
	Logs string
}

// GetInstanceLogs fetches the container logs for a given instance
func GetInstanceLogs(ctx context.Context, deps *Dependencies, params GetLogsParams) (*GetLogsResult, error) {
	instance, err := deps.Store.Instances.Get(params.InstanceName)
	if err != nil {
		return nil, operr.NotFoundf("instance not found: %s", params.InstanceName)
	}

	if instance.ContainerID == "" {
		return nil, operr.NotFoundf("instance %s has no container", params.InstanceName)
	}

	tail := params.Tail
	if tail == "" {
		tail = "100"
	}

	logs, err := deps.Docker.GetContainerLogs(instance.ContainerID, tail)
	if err != nil {
		return nil, fmt.Errorf("get logs: %w", err)
	}

	return &GetLogsResult{Logs: logs}, nil
}

// StreamInstanceLogs streams container logs to w until ctx is cancelled or the container stops.
func StreamInstanceLogs(ctx context.Context, deps *Dependencies, params GetLogsParams, w io.Writer) error {
	instance, err := deps.Store.Instances.Get(params.InstanceName)
	if err != nil {
		return operr.NotFoundf("instance not found: %s", params.InstanceName)
	}

	if instance.ContainerID == "" {
		return operr.NotFoundf("instance %s has no container", params.InstanceName)
	}

	tail := params.Tail
	if tail == "" {
		tail = "100"
	}

	return deps.Docker.StreamContainerLogs(ctx, instance.ContainerID, tail, w)
}
