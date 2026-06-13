package operations

import (
	"context"
	"fmt"

	"github.com/hypersequent/oddk/internal/operr"
	"github.com/hypersequent/oddk/internal/store/backup"
)

// ListBackupsParams contains parameters for listing backups
type ListBackupsParams struct {
	InstanceName string
}

// ListBackupsResult contains the result of listing backups
type ListBackupsResult struct {
	Backups []backup.BackupRecord
}

// ListBackups lists all backups for an RDBMS instance with consistency checks
// If InstanceName is empty, lists all backups across all instances
func ListBackups(ctx context.Context, deps *Dependencies, params ListBackupsParams) (*ListBackupsResult, error) {
	// If instance name is empty, list all backups
	if params.InstanceName == "" {
		backups, err := deps.Store.Backup.ListAllBackups()
		if err != nil {
			return nil, fmt.Errorf("list all backups: %w", err)
		}
		return &ListBackupsResult{
			Backups: backups,
		}, nil
	}

	// Check if instance exists first
	instance, err := deps.Store.Instances.Get(params.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}
	if instance == nil {
		return nil, operr.NotFoundf("instance not found: %s", params.InstanceName)
	}

	// Use the store's backup functionality with consistency checks
	// This will automatically clean up orphaned records
	backups, err := deps.Store.Backup.ListBackups(params.InstanceName)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}

	return &ListBackupsResult{
		Backups: backups,
	}, nil
}
