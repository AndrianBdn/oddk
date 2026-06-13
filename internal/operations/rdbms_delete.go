package operations

import (
	"context"
	"fmt"
	"log"
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

	// Remove container (ignore errors since container might not exist)
	if err := op.deps.Docker.RemoveContainer(instance.ContainerID); err != nil {
		log.Printf("Error removing container: %v", err)
	}

	// Remove volume (ignore errors since volume might not exist)
	volumeName := fmt.Sprintf("oddk-data-%s", op.params.Name)
	if err := op.deps.Docker.RemoveVolume(volumeName); err != nil {
		log.Printf("Error removing volume: %v", err)
	}

	if err := op.deps.Store.Instances.Delete(op.params.Name); err != nil {
		return fmt.Errorf("delete instance from store: %w", err)
	}

	return nil
}
