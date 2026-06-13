//go:build oddk_debug

// Package operations: this file holds the debug-only backup time-shift
// operation. It is compiled in only under the `oddk_debug` build tag (used by
// the e2e suite) and never ships in production binaries.
package operations

import (
	"context"
	"fmt"
	"time"

	"github.com/hypersequent/oddk/internal/rfc3339time"
)

// BackupTimeShiftOp shifts a backup's timestamp into the past for testing
type BackupTimeShiftOp struct {
	deps     *Dependencies
	backupID int
	daysBack int
	result   map[string]any
}

// NewBackupTimeShiftOp creates a new backup time shift operation
func NewBackupTimeShiftOp(deps *Dependencies, backupID, daysBack int) *BackupTimeShiftOp {
	return &BackupTimeShiftOp{
		deps:     deps,
		backupID: backupID,
		daysBack: daysBack,
	}
}

func (op *BackupTimeShiftOp) Name() string {
	return fmt.Sprintf("BackupTimeShift[%d]", op.backupID)
}

func (op *BackupTimeShiftOp) Type() OpType {
	return OpTypeWrite
}

func (op *BackupTimeShiftOp) Execute(ctx context.Context) error {
	backup, err := op.deps.Store.Backup.GetBackupByID(op.backupID)
	if err != nil {
		return fmt.Errorf("get backup: %w", err)
	}
	if backup == nil {
		return fmt.Errorf("backup not found: %d", op.backupID)
	}

	// Calculate new timestamp
	oldTimestamp := backup.Timestamp.Time
	newTimestamp := oldTimestamp.AddDate(0, 0, -op.daysBack)

	query := `UPDATE backup_history SET timestamp = ? WHERE id = ?`
	_, err = op.deps.Store.Sqlx.Exec(query, rfc3339time.Time{Time: newTimestamp}, op.backupID)
	if err != nil {
		return fmt.Errorf("update backup timestamp: %w", err)
	}

	op.result = map[string]any{
		"backupId":     op.backupID,
		"oldTimestamp": oldTimestamp.Format(time.RFC3339),
		"newTimestamp": newTimestamp.Format(time.RFC3339),
		"daysShifted":  op.daysBack,
		"message":      fmt.Sprintf("Backup %d shifted %d days into the past", op.backupID, op.daysBack),
	}

	return nil
}

func (op *BackupTimeShiftOp) GetResult() map[string]any {
	return op.result
}
