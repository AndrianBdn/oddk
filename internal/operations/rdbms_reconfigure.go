package operations

import (
	"context"
	"fmt"
	"log"

	"github.com/hypersequent/oddk/internal/crypto"
	"github.com/hypersequent/oddk/internal/operr"
	"github.com/hypersequent/oddk/internal/store/instances"
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

	// Check if the parameter group is different
	if instance.ParameterGroup == op.params.ParameterGroup {
		return operr.Invalidf("instance already uses parameter group: %s", op.params.ParameterGroup)
	}

	parameterGroup, err := op.deps.Store.Parameters.GetGroup(op.params.ParameterGroup)
	if err != nil {
		return operr.Invalidf("get parameter group %s: %w", op.params.ParameterGroup, err)
	}

	// Decrypt password (needed for container recreation)
	password, err := crypto.DecryptPassword(instance.Password, op.deps.MasterKey)
	if err != nil {
		return fmt.Errorf("decrypt password: %w", err)
	}

	if err := op.deps.Store.Instances.UpdateStatus(op.params.Name, "reconfiguring"); err != nil {
		log.Printf("Error updating status to reconfiguring: %v", err)
	}

	// Note: Health check coordination (pause/cleanup) is handled by the daemon/server layer
	// before calling this operation

	// Recreate the container with new parameters
	newContainerID, err := op.deps.Docker.RecreateContainer(
		op.params.Name,
		instance.Version,
		instance.Image,
		instance.Port,
		password,
		instance.CPUCores,
		instance.RAMMB,
		op.params.ParameterGroup,
		parameterGroup.Parameters,
		instance.ContainerID,
	)
	if err != nil {
		// Try to restore the old status
		if statusErr := op.deps.Store.Instances.UpdateStatus(op.params.Name, "error"); statusErr != nil {
			log.Printf("Error updating status to error: %v", statusErr)
		}
		return fmt.Errorf("recreate container: %w", err)
	}

	if err := op.deps.Store.Instances.UpdateContainerID(op.params.Name, newContainerID); err != nil {
		log.Printf("Error updating container ID: %v", err)
	}

	if err := op.deps.Store.Instances.UpdateParameterGroup(op.params.Name, op.params.ParameterGroup); err != nil {
		log.Printf("Error updating parameter group: %v", err)
	}

	if err := op.deps.Docker.StartContainer(newContainerID); err != nil {
		if statusErr := op.deps.Store.Instances.UpdateStatus(op.params.Name, "error"); statusErr != nil {
			log.Printf("Error updating status to error: %v", statusErr)
		}
		return fmt.Errorf("start container: %w", err)
	}

	// Wait for the recreated container to actually accept connections before
	// reporting "running".
	if err := waitForPostgresReady(ctx, instance.Port, password); err != nil {
		if statusErr := op.deps.Store.Instances.UpdateStatus(op.params.Name, "error"); statusErr != nil {
			log.Printf("Error updating status to error: %v", statusErr)
		}
		return fmt.Errorf("wait for PostgreSQL readiness: %w", err)
	}

	if err := op.deps.Store.Instances.UpdateStatus(op.params.Name, "running"); err != nil {
		log.Printf("Error updating status to running: %v", err)
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
