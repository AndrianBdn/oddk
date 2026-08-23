package operations

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store/instances"
)

// ReconfigureRDBMSParams contains the parameters for reconfiguring an RDBMS instance
type ReconfigureRDBMSParams struct {
	Name           string
	ParameterGroup string
}

// ReconfigureRDBMSResult contains the result of reconfiguring an RDBMS instance
type ReconfigureRDBMSResult struct {
	Instance *instances.RDBMSInstance
}

// ReconfigureRDBMSOp reconfigures an existing RDBMS instance with a new parameter group
type ReconfigureRDBMSOp struct {
	deps   *Dependencies
	params ReconfigureRDBMSParams
	result *ReconfigureRDBMSResult
}

// NewReconfigureRDBMSOp creates a new reconfigure RDBMS operation
func NewReconfigureRDBMSOp(deps *Dependencies, params ReconfigureRDBMSParams) *ReconfigureRDBMSOp {
	return &ReconfigureRDBMSOp{
		deps:   deps,
		params: params,
	}
}

func (op *ReconfigureRDBMSOp) Name() string {
	return fmt.Sprintf("ReconfigureRDBMS[%s]", op.params.Name)
}

func (op *ReconfigureRDBMSOp) Type() OpType {
	return OpTypeWrite
}

func (op *ReconfigureRDBMSOp) Execute(ctx context.Context) error {
	instance, err := op.deps.Store.Instances.Get(op.params.Name)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}

	// Same group on a healthy instance is a no-op. Same group in "error" is a
	// retry after a failed apply (the store may already record the new group,
	// or a rollback restored the old one — either way Recreate is the recovery).
	if instance.ParameterGroup == op.params.ParameterGroup && instance.Status != instances.StatusError {
		return operr.Invalidf("instance already uses parameter group: %s", op.params.ParameterGroup)
	}

	parameterGroup, err := op.deps.Store.Parameters.GetGroup(op.params.ParameterGroup)
	if err != nil {
		return operr.Invalidf("get parameter group %s: %w", op.params.ParameterGroup, err)
	}

	prevGroup, err := op.deps.Store.Parameters.GetGroup(instance.ParameterGroup)
	if err != nil {
		return fmt.Errorf("get current parameter group %s: %w", instance.ParameterGroup, err)
	}

	password, err := crypto.DecryptPassword(instance.Password, op.deps.MasterKey)
	if err != nil {
		return fmt.Errorf("decrypt password: %w", err)
	}

	prev := specFromInstance(instance, password, prevGroup)
	next := specFromInstance(instance, password, parameterGroup)
	next.ParameterGroup = op.params.ParameterGroup

	prevStatus := instance.Status
	if err := op.deps.Store.Instances.UpdateStatus(op.params.Name, instances.StatusReconfiguring); err != nil {
		log.Printf("Error updating status to reconfiguring: %v", err)
	}

	rolledBack, err := applyClusterChange(ctx, op.deps, next, prev, instance.ContainerID)
	if rolledBack {
		return fmt.Errorf("parameter group %s could not start: %w (rolled back to %s)",
			op.params.ParameterGroup, err, instance.ParameterGroup)
	}
	if err != nil {
		if prevStatus != instances.StatusError && errors.Is(err, operr.ErrInvalid) {
			if statusErr := op.deps.Store.Instances.UpdateStatus(op.params.Name, prevStatus); statusErr != nil {
				log.Printf("Error restoring status after preflight refusal: %v", statusErr)
			}
		}
		return err
	}

	// Commit the group only after PostgreSQL is accepting connections. Doing
	// this before start meant a failed apply recorded the bad group, so
	// re-applying it was refused and recovery required a *different* group.
	if err := op.deps.Store.Instances.UpdateParameterGroup(op.params.Name, op.params.ParameterGroup); err != nil {
		return fmt.Errorf("record parameter group: %w", err)
	}
	if err := op.deps.Store.Instances.UpdateStatus(op.params.Name, instances.StatusRunning); err != nil {
		return fmt.Errorf("record running status: %w", err)
	}

	instance, err = op.deps.Store.Instances.Get(op.params.Name)
	if err != nil {
		return fmt.Errorf("get updated instance: %w", err)
	}

	op.result = &ReconfigureRDBMSResult{
		Instance: instance,
	}

	return nil
}

// GetResult returns the operation result
func (op *ReconfigureRDBMSOp) GetResult() *ReconfigureRDBMSResult {
	return op.result
}
