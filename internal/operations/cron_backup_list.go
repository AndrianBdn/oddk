package operations

import (
	"context"
	"fmt"

	"github.com/hypersequent/oddk/internal/store/cron"
)

type CronBackupListOp struct {
	deps   *Dependencies
	result []*cron.CronPlan
}

func NewCronBackupListOp(deps *Dependencies) *CronBackupListOp {
	return &CronBackupListOp{deps: deps}
}

func (op *CronBackupListOp) Name() string {
	return "CronBackupList"
}

func (op *CronBackupListOp) Type() OpType {
	return OpTypeRead
}

func (op *CronBackupListOp) Execute(ctx context.Context) error {
	plans, err := op.deps.Store.Cron.ListPlans()
	if err != nil {
		return fmt.Errorf("listing cron plans: %w", err)
	}

	op.result = plans
	return nil
}

func (op *CronBackupListOp) GetResult() []*cron.CronPlan {
	return op.result
}
