package operations

import (
	"fmt"

	"github.com/andrianbdn/oddk/internal/store/cron"
)

// CronLogsParams selects which scheduled-run history to return.
type CronLogsParams struct {
	// Instance filters to one instance's runs. Empty returns every instance's,
	// including the whole-deployment snapshot runs recorded under
	// SnapshotCronInstance.
	Instance string
	Limit    int
}

// CronLogsResult is the scheduled-run history.
type CronLogsResult struct {
	Logs []*cron.CronLog `json:"logs"`
}

// CronLogs returns the recorded history of scheduled backups and snapshots.
//
// cron_logs is the evidence that scheduled protection actually ran — the table
// the 365-day retention window exists to preserve for SOC 2 / PCI sampling — and
// until this operation existed it had no reader anywhere in the API or the CLI.
// A failed run recorded its reason and returned nil, leaving that reason
// reachable only through journalctl or a manual sqlite3 session.
func CronLogs(deps *Dependencies, params CronLogsParams) (*CronLogsResult, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}

	var (
		logs []*cron.CronLog
		err  error
	)
	if params.Instance != "" {
		logs, err = deps.Store.Cron.ListLogs(params.Instance, limit)
	} else {
		logs, err = deps.Store.Cron.ListAllLogs(limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list cron logs: %w", err)
	}

	if logs == nil {
		logs = []*cron.CronLog{}
	}
	return &CronLogsResult{Logs: logs}, nil
}
