package operations

import (
	"context"
	"fmt"
	"strings"

	"github.com/hypersequent/oddk/internal/store/notifications"
)

type NotificationGetParams struct {
	Name string `json:"name"`
}

type NotificationGetResult struct {
	Notification *notifications.Notification `json:"notification"`
}

func NotificationGet(ctx context.Context, deps *Dependencies, params NotificationGetParams) (*NotificationGetResult, error) {
	if templateType, ok := strings.CutPrefix(params.Name, "oddk:template:"); ok {

		if err := notifications.ValidateNotificationType(templateType); err != nil {
			return nil, fmt.Errorf("invalid template type: %w", err)
		}

		templateNotification, err := notifications.GetTemplate(notifications.NotificationType(templateType), "")
		if err != nil {
			return nil, fmt.Errorf("failed to get template: %w", err)
		}

		return &NotificationGetResult{
			Notification: templateNotification,
		}, nil
	}

	notifStore := deps.Store.Notifications
	notification, err := notifStore.Get(params.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get notification: %w", err)
	}

	return &NotificationGetResult{
		Notification: notification,
	}, nil
}
