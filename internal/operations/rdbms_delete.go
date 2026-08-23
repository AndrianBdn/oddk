package operations

import (
	"context"
	"fmt"

	"github.com/andrianbdn/oddk/internal/docker"
)

// DeleteRDBMSOp deletes an RDBMS instance
type DeleteRDBMSOp struct {
	deps   *Dependencies
	params DeleteRDBMSParams
}

func NewDeleteRDBMSOp(deps *Dependencies, params DeleteRDBMSParams) *DeleteRDBMSOp {
	return &DeleteRDBMSOp{deps: deps, params: params}
}

func (op *DeleteRDBMSOp) Name() string {
	return fmt.Sprintf("DeleteRDBMS[%s]", op.params.Name)
}

func (op *DeleteRDBMSOp) Type() OpType {
	return OpTypeWrite
}

func (op *DeleteRDBMSOp) Execute(ctx context.Context) error {
	instance, err := op.deps.Store.Instances.Get(op.params.Name)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}

	// Do not drop the catalogue row until Docker cleanup succeeds. Create
	// refuses to adopt an existing volume, so a "successful" destroy that left
	// the volume behind makes `oddk create --name <same>` fail forever.
	if instance.ContainerID != "" {
		if err := op.deps.Docker.RemoveContainer(instance.ContainerID); err != nil {
			return fmt.Errorf("remove container: %w (instance was not deleted; retry destroy)", err)
		}
	}
	// A crash between create and UpdateContainerID can leave oddk-pg-<name>
	// with an ID the store does not know. Remove by name too; no-op if gone.
	containerName := docker.ContainerName(op.params.Name)
	if err := op.deps.Docker.RemoveContainer(containerName); err != nil {
		return fmt.Errorf("remove container %s: %w (instance was not deleted; retry destroy)", containerName, err)
	}
	volumeName := docker.VolumeName(op.params.Name)
	if err := op.deps.Docker.RemoveVolume(volumeName); err != nil {
		return fmt.Errorf("remove volume %s: %w (instance was not deleted; retry destroy)", volumeName, err)
	}

	if err := op.deps.Store.Instances.Delete(op.params.Name); err != nil {
		return fmt.Errorf("delete instance from store: %w", err)
	}

	return nil
}
