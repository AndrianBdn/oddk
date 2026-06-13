package operations

import (
	"context"
	"fmt"
)

type NotificationRemoveParams struct {
	Name string `json:"name"`
}

type NotificationRemoveResult struct {
	Message string `json:"message"`
}

func NotificationRemove(ctx context.Context, deps *Dependencies, params NotificationRemoveParams) (*NotificationRemoveResult, error) {
	notifStore := deps.Store.Notifications

	if err := notifStore.Delete(params.Name); err != nil {
		return nil, fmt.Errorf("failed to remove notification: %w", err)
	}

	deps.Logger.Printf("Removed notification: %s", params.Name)

	return &NotificationRemoveResult{
		Message: fmt.Sprintf("Notification %s removed successfully", params.Name),
	}, nil
}
