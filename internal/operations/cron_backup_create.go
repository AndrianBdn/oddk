package operations

import (
	"context"
	"fmt"

	"github.com/hypersequent/oddk/internal/store/cron"
)

type CronBackupCreateOp struct {
	deps              *Dependencies
	instanceName      string
	utcHour           int
	cleanupLocalDays  int
	cleanupRemoteDays int
	result            *cron.CronPlan
}

func NewCronBackupCreateOp(deps *Dependencies, instanceName string, utcHour, cleanupLocalDays, cleanupRemoteDays int) *CronBackupCreateOp {
	return &CronBackupCreateOp{
		deps:              deps,
		instanceName:      instanceName,
		utcHour:           utcHour,
		cleanupLocalDays:  cleanupLocalDays,
		cleanupRemoteDays: cleanupRemoteDays,
	}
}

func (op *CronBackupCreateOp) Name() string {
	return "CronBackupCreate"
}

func (op *CronBackupCreateOp) Type() OpType {
	return OpTypeWrite
}

func (op *CronBackupCreateOp) Execute(ctx context.Context) error {
	instance, err := op.deps.Store.Instances.Get(op.instanceName)
	if err != nil {
		return fmt.Errorf("getting instance: %w", err)
	}
	if instance == nil {
		return fmt.Errorf("instance '%s' not found", op.instanceName)
	}

	if err := op.deps.Store.Cron.CreatePlan(op.instanceName, op.utcHour, op.cleanupLocalDays, op.cleanupRemoteDays); err != nil {
		return fmt.Errorf("creating cron plan: %w", err)
	}

	plan, err := op.deps.Store.Cron.GetPlan(op.instanceName)
	if err != nil {
		return fmt.Errorf("getting created cron plan: %w", err)
	}

	op.result = plan
	return nil
}

func (op *CronBackupCreateOp) GetResult() *cron.CronPlan {
	return op.result
}
