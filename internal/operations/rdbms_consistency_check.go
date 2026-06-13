package operations

import (
	"context"
	"fmt"

	"github.com/hypersequent/oddk/internal/store/instances"
)

// ConsistencyStatus represents the health status of an RDBMS instance
type ConsistencyStatus struct {
	ContainerExists  bool
	ContainerRunning bool
	PostgreSQLReady  bool
	OverallHealthy   bool
	Issues           []string
}

// ConsistencyCheckOp validates RDBMS instance consistency
type ConsistencyCheckOp struct {
	deps     *Dependencies
	instance *instances.RDBMSInstance
	status   *ConsistencyStatus
}

func NewConsistencyCheckOp(deps *Dependencies, instance *instances.RDBMSInstance) *ConsistencyCheckOp {
	return &ConsistencyCheckOp{
		deps:     deps,
		instance: instance,
		status: &ConsistencyStatus{
			Issues: make([]string, 0),
		},
	}
}

func (op *ConsistencyCheckOp) Name() string {
	return fmt.Sprintf("ConsistencyCheck[%s]", op.instance.Name)
}

func (op *ConsistencyCheckOp) Type() OpType {
	return OpTypeRead
}

func (op *ConsistencyCheckOp) Execute(ctx context.Context) error {
	// Check if container exists
	containerStatus, err := op.deps.Docker.GetContainerStatus(op.instance.ContainerID)
	if err != nil {
		op.status.ContainerExists = false
		op.status.Issues = append(op.status.Issues, fmt.Sprintf("container %s does not exist", op.instance.ContainerID))
		op.instance.Status = "broken"
		return nil // Continue checking other aspects
	}
	op.status.ContainerExists = true

	// Check if container is running
	if containerStatus == "running" {
		op.status.ContainerRunning = true
	} else {
		op.status.ContainerRunning = false
		op.status.Issues = append(op.status.Issues, fmt.Sprintf("container is %s, not running", containerStatus))
		op.instance.Status = containerStatus
	}

	// Check PostgreSQL connectivity (only if container is running)
	if op.status.ContainerRunning {
		pgStatus := op.checkPostgreSQLConnectivity(ctx)
		op.status.PostgreSQLReady = (pgStatus == PostgreSQLStatusOK)

		switch pgStatus {
		case PostgreSQLStatusOK:
			// All good, no action needed
		case PostgreSQLStatusBrokenPort:
			op.status.Issues = append(op.status.Issues, "PostgreSQL port is not accessible")
			op.instance.Status = "broken-port"
		case PostgreSQLStatusBrokenAuth:
			op.status.Issues = append(op.status.Issues, "PostgreSQL authentication failed")
			op.instance.Status = "broken-auth"
		case PostgreSQLStatusOther:
			op.status.Issues = append(op.status.Issues, "PostgreSQL connectivity issue (other)")
			op.instance.Status = "broken"
		}
	}

	// Determine overall health
	op.status.OverallHealthy = op.status.ContainerExists &&
		op.status.ContainerRunning &&
		op.status.PostgreSQLReady

	// Update status in database if needed
	if op.instance.Status != containerStatus && op.status.ContainerExists {
		if err := op.deps.Store.Instances.UpdateStatus(op.instance.Name, op.instance.Status); err != nil {
			return fmt.Errorf("update status: %w", err)
		}
	}

	return nil
}

func (op *ConsistencyCheckOp) checkPostgreSQLConnectivity(ctx context.Context) PostgreSQLStatus {
	return TestPostgreSQLConnectivity(ctx, op.deps, op.instance.Name)
}

func (op *ConsistencyCheckOp) GetStatus() *ConsistencyStatus {
	return op.status
}

func (op *ConsistencyCheckOp) GetInstance() *instances.RDBMSInstance {
	return op.instance
}
