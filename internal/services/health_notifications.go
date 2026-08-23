package services

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/andrianbdn/oddk/internal/store"
	"github.com/andrianbdn/oddk/internal/store/health"
	"github.com/andrianbdn/oddk/internal/store/kvstore"
)

// NotificationState represents the current notification state
type NotificationState int

const (
	StateUnknown NotificationState = iota
	StateHealthy
	StateDegraded
)

func (s NotificationState) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateDegraded:
		return "degraded"
	default:
		return "unknown"
	}
}

// HealthNotificationEvaluator evaluates health records and determines if notifications should be sent
type HealthNotificationEvaluator struct {
	store             *store.Store
	lastNotifiedState NotificationState
}

func NewHealthNotificationEvaluator(store *store.Store) *HealthNotificationEvaluator {
	return &HealthNotificationEvaluator{
		store:             store,
		lastNotifiedState: StateUnknown,
	}
}

// EvaluateAndNotify checks if notification thresholds are met and sends notifications if needed
func (e *HealthNotificationEvaluator) EvaluateAndNotify(ctx context.Context) error {
	degradedThreshold := e.store.KV.RequiredInt(kvstore.KeyHealthDegradedThreshold)
	restoredThreshold := e.store.KV.RequiredInt(kvstore.KeyHealthRestoredThreshold)

	records, err := e.store.Health.GetRecentHealthRecords(max(degradedThreshold, restoredThreshold))
	if err != nil {
		return fmt.Errorf("get recent health records: %w", err)
	}

	if len(records) == 0 {
		return nil // No records to evaluate
	}

	currentState := e.evaluateHealthState(records, degradedThreshold, restoredThreshold)

	// Determine if we need to send a notification
	shouldNotify, notificationType := e.shouldSendNotification(currentState)

	if shouldNotify {
		if err := e.sendHealthNotification(ctx, notificationType, records[0]); err != nil {
			return fmt.Errorf("send health notification: %w", err)
		}
		e.lastNotifiedState = currentState
	}

	return nil
}

// evaluateHealthState determines the current health state based on recent records
func (e *HealthNotificationEvaluator) evaluateHealthState(records []*health.HealthRecord, degradedThreshold, restoredThreshold int) NotificationState {
	if len(records) == 0 {
		return StateUnknown
	}

	// Check for degraded state: degradedThreshold consecutive failures
	if len(records) >= degradedThreshold {
		allFailed := true
		for i := range degradedThreshold {
			if records[i].HealthyAll {
				allFailed = false
				break
			}
		}
		if allFailed {
			return StateDegraded
		}
	}

	// Check for healthy state: restoredThreshold consecutive successes
	if len(records) >= restoredThreshold {
		allHealthy := true
		for i := range restoredThreshold {
			if !records[i].HealthyAll {
				allHealthy = false
				break
			}
		}
		if allHealthy {
			return StateHealthy
		}
	}

	// Mixed state or insufficient data
	return StateUnknown
}

// shouldSendNotification determines if a notification should be sent based on state transitions
func (e *HealthNotificationEvaluator) shouldSendNotification(currentState NotificationState) (bool, string) {
	// Don't send duplicate notifications
	if currentState == e.lastNotifiedState {
		return false, ""
	}

	switch currentState {
	case StateDegraded:
		if e.lastNotifiedState != StateDegraded {
			return true, "service_degraded"
		}
	case StateHealthy:
		if e.lastNotifiedState == StateDegraded {
			return true, "service_restored"
		}
	}

	return false, ""
}

// sendHealthNotification sends notifications to all configured channels with retry logic
func (e *HealthNotificationEvaluator) sendHealthNotification(ctx context.Context, notificationType string, latestRecord *health.HealthRecord) error {
	notifications, err := e.store.Notifications.List()
	if err != nil {
		return fmt.Errorf("list notifications: %w", err)
	}

	if len(notifications) == 0 {
		log.Printf("No notifications configured, skipping %s notification", notificationType)
		return nil
	}

	// Prepare notification message
	message := e.buildNotificationMessage(notificationType, latestRecord)

	// Send to all channels with retry logic
	var lastError error
	successCount := 0

	for _, notification := range notifications {
		if err := e.sendWithRetries(ctx, notification.Name, message); err != nil {
			log.Printf("Failed to send %s notification to %s: %v", notificationType, notification.Name, err)
			lastError = err
		} else {
			successCount++
		}
	}

	log.Printf("Health notification (%s) sent to %d/%d channels", notificationType, successCount, len(notifications))

	// Return error if all notifications failed
	if successCount == 0 && lastError != nil {
		return fmt.Errorf("all notification channels failed: %w", lastError)
	}

	return nil
}

const (
	// instanceErrorPrefix is how performInstanceChecks reports an instance whose
	// STORED status is "error" — a fact from the store, not a failed probe.
	instanceErrorPrefix = "instance_error:"

	// instanceErrorHint separates the two ways an instance reaches "error",
	// because they need opposite responses: a write operation that failed leaves
	// a cluster that must be repaired, while `snapshot apply` deliberately parks
	// configuration-only instances there and the operator has already been told
	// so at the terminal. Without this the channel says only "Broken Instances:
	// foo", which reads identically to a cluster that just died.
	instanceErrorHint = "\nAn instance in 'error' state is not being probed — it is recorded as broken.\n" +
		"That is either a failed operation (apply/switch/start) that needs repair, or a\n" +
		"configuration-only instance left behind by 'snapshot apply'. Run 'oddk checklist'.\n"
)

// buildNotificationMessage creates the notification message content
func (e *HealthNotificationEvaluator) buildNotificationMessage(notificationType string, record *health.HealthRecord) string {
	timestamp := time.Unix(record.TsUnix, 0).Format("2006-01-02 15:04:05 UTC")
	displayName := e.store.KV.GetDisplayName()

	switch notificationType {
	case "service_degraded":
		return buildDegradedMessage(displayName, timestamp, record)

	case "service_restored":
		message := fmt.Sprintf("✅ ODDK Service Restored (%s)\n\n", displayName)
		message += fmt.Sprintf("Time: %s\n", timestamp)

		if record.HealthyInstances != "" {
			message += fmt.Sprintf("Healthy Instances: %s\n", record.HealthyInstances)
		} else {
			message += "All systems are now healthy.\n"
		}

		message += "\nService has been restored to normal operation."
		return message

	default:
		return fmt.Sprintf("ODDK Health Status Update (%s): %s at %s", displayName, notificationType, timestamp)
	}
}

// buildDegradedMessage renders the degraded body. Split out from
// buildNotificationMessage so it can be tested without a store: this formatting
// is where a real defect hid unnoticed, because nothing asserted what the
// operator actually receives.
func buildDegradedMessage(displayName, timestamp string, record *health.HealthRecord) string {
	message := fmt.Sprintf("🚨 ODDK Service Degraded (%s)\n\n", displayName)
	message += fmt.Sprintf("Time: %s\n", timestamp)

	if !record.HealthyHost {
		message += "Host: unhealthy\n"
	}
	if record.BrokenInstances != "" {
		message += fmt.Sprintf("Broken Instances: %s\n", record.BrokenInstances)
	}
	// fail_details is ONE comma-joined string carrying both the host and the
	// instance messages (see the run loop in health_checker.go), so it can be
	// neither labelled "Host Issues" nor shown only when the host is unhealthy.
	// It used to be both: an instance failure was dropped entirely when the host
	// was fine, and mislabelled as a host issue when it was not. It is the only
	// part of the message that says WHY, and the reasons need different
	// responses — see instanceErrorHint.
	if record.FailDetails != "" {
		message += fmt.Sprintf("Details: %s\n", record.FailDetails)
	}
	if strings.Contains(record.FailDetails, instanceErrorPrefix) {
		message += instanceErrorHint
	}

	message += "\nPlease check the ODDK daemon for more details."
	return message
}

// sendWithRetries sends a notification with retry logic
func (e *HealthNotificationEvaluator) sendWithRetries(ctx context.Context, name, message string) error {
	const maxRetries = 3
	const retryDelay = 2 * time.Second

	var lastError error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		sender := NewNotificationSender(e.store.Notifications, e.store)

		if err := sender.Send(ctx, name, "ODDK Health Status", message); err != nil {
			lastError = err
			log.Printf("Notification attempt %d/%d failed for %s: %v", attempt, maxRetries, name, err)

			if attempt < maxRetries {
				select {
				case <-time.After(retryDelay):
					continue
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		} else {
			log.Printf("Successfully sent health notification to %s on attempt %d", name, attempt)
			return nil
		}
	}

	return fmt.Errorf("failed after %d attempts: %w", maxRetries, lastError)
}
