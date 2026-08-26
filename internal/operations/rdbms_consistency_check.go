package operations

import (
	"context"
	"fmt"

	"github.com/andrianbdn/oddk/internal/store/instances"
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

// Execute inspects the instance and reports what it finds. It WRITES NOTHING.
//
// It is declared OpTypeRead and reached from the three most-run read commands
// (`oddk list`, `oddk instance status`, `oddk checklist`), and it runs on the
// executor's lock-free read path — so writing here would both break the
// OpTypeRead contract and race with whatever write operation is in flight.
//
// It used to persist. First the PostgreSQL probe verdict, which latched: one
// blip wrote "broken-port" from an ordinary `oddk list`, and the recovery path
// had no branch that wrote "running" back, so every later run observed a healthy
// server and re-persisted the stale value. That was narrowed in 0.1.66 to
// container-derived state only, and is now removed entirely — because even the
// narrow version had the same shape of bug: an out-of-band `docker stop`,
// once observed by any `oddk list`, wrote "stopped" and thereby removed the
// instance from health monitoring (the checker skips anything not "running"), so
// no degraded notification could ever fire for it. A read command silently
// switching off monitoring is exactly the disease this file caught.
//
// Startup reconciliation owns store correction now (see daemon.decideReconcile).
// Between restarts a stale row is harmless: snapshot capture asks Docker
// directly, the checklist and `oddk list` display the corrected status computed
// here in memory, and an instance whose container really did stop now fails its
// health check and alerts — which is the truthful outcome.
//
// The one status the container does NOT get to correct is "error" — see
// deriveDisplayStatus.
func (op *ConsistencyCheckOp) Execute(ctx context.Context) error {
	// Sampled before anything below overwrites it.
	stored := op.instance.Status

	containerState, err := op.deps.Docker.GetContainerStatus(op.instance.ContainerID)
	if err != nil {
		op.status.ContainerExists = false
		op.status.Issues = append(op.status.Issues, fmt.Sprintf("container state could not be determined: %v", err))
		return nil // Continue checking other aspects
	}

	if containerState == "not found" {
		op.status.ContainerExists = false
		op.status.Issues = append(op.status.Issues, fmt.Sprintf("container %s does not exist", op.instance.ContainerID))
		op.instance.Status = instances.StatusError
		return nil
	}
	op.status.ContainerExists = true

	// Check if container is running
	if containerState == "running" {
		op.status.ContainerRunning = true
	} else {
		op.status.ContainerRunning = false
		op.status.Issues = append(op.status.Issues, fmt.Sprintf("container is %s, not running", containerState))
		op.instance.Status = deriveDisplayStatus(stored, instances.InstanceStatus(containerState))
	}

	// Check PostgreSQL connectivity (only if container is running)
	if op.status.ContainerRunning {
		pgStatus := op.checkPostgreSQLConnectivity(ctx)
		op.status.PostgreSQLReady = (pgStatus == PostgreSQLStatusOK)

		switch pgStatus {
		case PostgreSQLStatusOK:
			op.instance.Status = deriveDisplayStatus(stored, instances.StatusRunning)
		case PostgreSQLStatusBrokenPort:
			op.status.Issues = append(op.status.Issues, "PostgreSQL port is not accessible")
			op.instance.Status = deriveDisplayStatus(stored, instances.StatusBrokenPort)
		case PostgreSQLStatusBrokenAuth:
			op.status.Issues = append(op.status.Issues, "PostgreSQL authentication failed")
			op.instance.Status = deriveDisplayStatus(stored, instances.StatusBrokenAuth)
		case PostgreSQLStatusOther:
			op.status.Issues = append(op.status.Issues, "PostgreSQL connectivity issue (other)")
			op.instance.Status = deriveDisplayStatus(stored, instances.StatusBroken)
		}
	}

	// Determine overall health
	op.status.OverallHealthy = op.status.ContainerExists &&
		op.status.ContainerRunning &&
		op.status.PostgreSQLReady

	return nil
}

// deriveDisplayStatus resolves what a read command should SHOW for an instance,
// given the status stored in its row and the one the container implies.
//
// "error" wins. It is terminal: it is only ever written by an operation that
// failed, and only an explicit operation promotes out of it — reconciliation
// deliberately never auto-clears it (see instances.StatusError and
// daemon.decideReconcile). A container that exists, or is up, or even answers a
// connect, does not refute it; that is the whole reason "restoring" resolves to
// "error" rather than to whatever its half-restored server reports.
//
// Without this the row and the screen disagreed, permanently rather than
// transiently: an apply whose rollback also failed stores "error", and `oddk
// list` then rendered the exited container as a plain "stopped" — the word for
// an instance somebody stopped on purpose — while the same `oddk checklist`
// run printed "instance_error:<name>" and "health failing" two lines apart.
// Nothing clears the row, so that read every time, forever.
//
// Every OTHER stored status still defers to the container, which is what this
// file is for: a row saying "running" for a container somebody stopped out of
// band must read "stopped". The interrupted statuses (creating, switching, ...)
// defer too, and deliberately — from here a dead operation and a live one look
// identical, which is exactly why reconciliation resolves those at startup,
// where they don't.
func deriveDisplayStatus(stored, fromContainer instances.InstanceStatus) instances.InstanceStatus {
	if stored == instances.StatusError {
		return instances.StatusError
	}
	return fromContainer
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
