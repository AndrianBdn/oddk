package operations

import (
	"context"
	"fmt"
)

type CronBackupDeleteOp struct {
	deps         *Dependencies
	instanceName string
}

func NewCronBackupDeleteOp(deps *Dependencies, instanceName string) *CronBackupDeleteOp {
	return &CronBackupDeleteOp{
		deps:         deps,
		instanceName: instanceName,
	}
}

func (op *CronBackupDeleteOp) Name() string {
	return "CronBackupDelete"
}

func (op *CronBackupDeleteOp) Type() OpType {
	return OpTypeWrite
}

func (op *CronBackupDeleteOp) Execute(ctx context.Context) error {
	// Check if the cron plan exists
	exists, err := op.deps.Store.Cron.CheckPlanExists(op.instanceName)
	if err != nil {
		return fmt.Errorf("checking cron plan existence: %w", err)
	}

	if !exists {
		return fmt.Errorf("no scheduled backup found for instance '%s'", op.instanceName)
	}

	if err := op.deps.Store.Cron.DeletePlan(op.instanceName); err != nil {
		return fmt.Errorf("deleting cron plan: %w", err)
	}

	return nil
}
