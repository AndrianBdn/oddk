package operations

import (
	"context"
	"fmt"
	"log"

	"github.com/hypersequent/oddk/internal/store/instances"
)

// GetRDBMSOp gets a single RDBMS instance
type GetRDBMSOp struct {
	deps   *Dependencies
	name   string
	result *instances.RDBMSInstance
}

func NewGetRDBMSOp(deps *Dependencies, name string) *GetRDBMSOp {
	return &GetRDBMSOp{deps: deps, name: name}
}

func (op *GetRDBMSOp) Name() string {
	return fmt.Sprintf("GetRDBMS[%s]", op.name)
}

func (op *GetRDBMSOp) Type() OpType {
	return OpTypeRead
}

func (op *GetRDBMSOp) Execute(ctx context.Context) error {
	instance, err := op.deps.Store.Instances.Get(op.name)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}

	checkOp := NewConsistencyCheckOp(op.deps, instance)
	if err := checkOp.Execute(ctx); err != nil {
		log.Printf("Error running consistency check: %v", err)
		// Continue even if check fails - we still want to return the instance
	}

	op.result = checkOp.GetInstance()

	status := checkOp.GetStatus()
	if !status.OverallHealthy && len(status.Issues) > 0 {
		log.Printf("Instance %s has consistency issues: %v", op.name, status.Issues)
	}

	return nil
}

func (op *GetRDBMSOp) GetResult() *instances.RDBMSInstance {
	return op.result
}
