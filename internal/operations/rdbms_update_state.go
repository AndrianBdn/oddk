package operations

import (
	"context"
	"fmt"
	"log"

	"github.com/hypersequent/oddk/internal/operr"
)

// UpdateStateOp updates the state of an RDBMS instance
type UpdateStateOp struct {
	deps   *Dependencies
	params UpdateStateParams
}

func NewUpdateStateOp(deps *Dependencies, params UpdateStateParams) *UpdateStateOp {
	return &UpdateStateOp{deps: deps, params: params}
}

func (op *UpdateStateOp) Name() string {
	return fmt.Sprintf("UpdateState[%s->%s]", op.params.Name, op.params.State)
}

func (op *UpdateStateOp) Type() OpType {
	return OpTypeWrite
}

func (op *UpdateStateOp) Execute(ctx context.Context) error {
	instance, err := op.deps.Store.Instances.Get(op.params.Name)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}

	switch op.params.State {
	case "start":
		if err := op.deps.Docker.StartContainer(instance.ContainerID); err != nil {
			return fmt.Errorf("start container: %w", err)
		}
		// Wait for PostgreSQL to accept connections before reporting "running".
		// The status stays non-running until then, so the health checker (which
		// reads status directly) won't ping a not-yet-ready cluster and flag it.
		password, err := op.deps.Store.Instances.GetDecryptedPassword(op.params.Name, op.deps.MasterKey)
		if err != nil {
			return fmt.Errorf("decrypt password: %w", err)
		}
		if err := waitForPostgresReady(ctx, instance.Port, password); err != nil {
			// Readiness timed out: the container is up but PostgreSQL never
			// answered within the window (rare for a manual start — stop flushes
			// WAL, so a normal restart is fast; this mostly means a genuinely
			// broken instance). Record "error" honestly rather than claim
			// "running". This is recoverable: re-running 'instance start' is safe
			// (StartContainer is a no-op on an already-running container) and will
			// promote the instance once PostgreSQL is actually ready. We don't
			// auto-promote from "error" here, matching how every other
			// non-"running" status in ODDK is cleared only by an explicit op.
			if statusErr := op.deps.Store.Instances.UpdateStatus(op.params.Name, "error"); statusErr != nil {
				log.Printf("Error updating status to error: %v", statusErr)
			}
			return fmt.Errorf("wait for PostgreSQL readiness: %w", err)
		}
		if err := op.deps.Store.Instances.UpdateStatus(op.params.Name, "running"); err != nil {
			log.Printf("Error updating RDBMS status to running: %v", err)
		}

	case "stop":
		if err := op.deps.Docker.StopContainer(instance.ContainerID); err != nil {
			return fmt.Errorf("stop container: %w", err)
		}
		if err := op.deps.Store.Instances.UpdateStatus(op.params.Name, "stopped"); err != nil {
			log.Printf("Error updating RDBMS status to stopped: %v", err)
		}

	default:
		return operr.Invalidf("invalid state: %s", op.params.State)
	}

	return nil
}
