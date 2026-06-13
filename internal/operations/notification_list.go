package operations

import (
	"context"

	"github.com/hypersequent/oddk/internal/store/notifications"
)

type NotificationListParams struct{}

type NotificationListResult struct {
	Notifications []notifications.Notification `json:"notifications"`
}

func NotificationList(ctx context.Context, deps *Dependencies, params NotificationListParams) (*NotificationListResult, error) {
	notifStore := deps.Store.Notifications

	notifs, err := notifStore.List()
	if err != nil {
		return nil, err
	}

	return &NotificationListResult{
		Notifications: notifs,
	}, nil
}
