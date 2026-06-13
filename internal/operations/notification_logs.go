package operations

import (
	"context"

	"github.com/hypersequent/oddk/internal/store/notifications"
)

type NotificationLogsParams struct {
	Limit int `json:"limit"`
}

type NotificationLogsResult struct {
	Logs []notifications.NotificationLog `json:"logs"`
}

func NotificationLogs(ctx context.Context, deps *Dependencies, params NotificationLogsParams) (*NotificationLogsResult, error) {
	notifStore := deps.Store.Notifications

	limit := params.Limit
	if limit <= 0 {
		limit = 100
	}

	logs, err := notifStore.GetLogs(limit)
	if err != nil {
		return nil, err
	}

	return &NotificationLogsResult{
		Logs: logs,
	}, nil
}
