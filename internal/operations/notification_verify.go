package operations

import (
	"context"
	"fmt"

	"github.com/hypersequent/oddk/internal/services"
)

type NotificationTestParams struct{}

type NotificationTestResult struct {
	Message string `json:"message"`
}

func NotificationTest(ctx context.Context, deps *Dependencies, params NotificationTestParams) (*NotificationTestResult, error) {
	notifStore := deps.Store.Notifications
	sender := services.NewNotificationSender(notifStore, deps.Store)

	if err := sender.Test(ctx); err != nil {
		return nil, fmt.Errorf("notification test failed: %w", err)
	}

	deps.Logger.Printf("Sent test notifications to all configured destinations")

	return &NotificationTestResult{
		Message: "Test notifications sent successfully",
	}, nil
}
