package operations

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hypersequent/oddk/internal/store/notifications"
)

type NotificationEditParams struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
}

type NotificationEditResult struct {
	Notification *notifications.Notification `json:"notification"`
}

func NotificationEdit(ctx context.Context, deps *Dependencies, params NotificationEditParams) (*NotificationEditResult, error) {
	if err := notifications.ValidateNotificationName(params.Name); err != nil {
		return nil, fmt.Errorf("invalid name: %w", err)
	}

	if err := notifications.ValidateNotificationType(params.Type); err != nil {
		return nil, err
	}

	notifType := notifications.NotificationType(params.Type)
	if err := notifications.ValidateConfig(notifType, params.Config); err != nil {
		return nil, err
	}

	notifStore := deps.Store.Notifications

	notification, err := notifStore.Update(params.Name, notifType, params.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to update notification: %w", err)
	}

	deps.Logger.Printf("Updated notification: %s", notification.Name)

	return &NotificationEditResult{
		Notification: notification,
	}, nil
}
