package operations

import (
	"context"
	"fmt"
	"log"

	"github.com/hypersequent/oddk/internal/store/instances"
)

// ListRDBMSOp lists all RDBMS instances
type ListRDBMSOp struct {
	deps   *Dependencies
	result []instances.RDBMSInstance
}

func NewListRDBMSOp(deps *Dependencies) *ListRDBMSOp {
	return &ListRDBMSOp{deps: deps}
}

func (op *ListRDBMSOp) Name() string {
	return "ListRDBMS"
}

func (op *ListRDBMSOp) Type() OpType {
	return OpTypeRead
}

func (op *ListRDBMSOp) Execute(ctx context.Context) error {
	instances, err := op.deps.Store.Instances.List()
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}

	for i := range instances {
		checkOp := NewConsistencyCheckOp(op.deps, &instances[i])
		if err := checkOp.Execute(ctx); err != nil {
			log.Printf("Error checking instance %s: %v", instances[i].Name, err)
			// Continue checking other instances
			continue
		}

		instances[i] = *checkOp.GetInstance()

		status := checkOp.GetStatus()
		if !status.OverallHealthy && len(status.Issues) > 0 {
			log.Printf("Instance %s has consistency issues: %v", instances[i].Name, status.Issues)
		}
	}

	op.result = instances
	return nil
}

func (op *ListRDBMSOp) GetResult() []instances.RDBMSInstance {
	return op.result
}
