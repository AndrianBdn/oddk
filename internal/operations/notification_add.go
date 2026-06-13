package operations

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hypersequent/oddk/internal/store/notifications"
)

type NotificationAddParams struct {
	Name   string                         `json:"name"`
	Type   notifications.NotificationType `json:"type"`
	Config json.RawMessage                `json:"config"`
}

type NotificationAddResult struct {
	Notification *notifications.Notification `json:"notification"`
}

func NotificationAdd(ctx context.Context, deps *Dependencies, params NotificationAddParams) (*NotificationAddResult, error) {
	if err := notifications.ValidateNotificationName(params.Name); err != nil {
		return nil, fmt.Errorf("invalid name: %w", err)
	}

	notifStore := deps.Store.Notifications

	notification, err := notifStore.Create(params.Name, params.Type, params.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to add notification: %w", err)
	}

	deps.Logger.Printf("Added notification: %s (type: %s)", notification.Name, notification.Type)

	return &NotificationAddResult{
		Notification: notification,
	}, nil
}
