package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-pkgz/notify"

	"github.com/hypersequent/oddk/internal/store"
	"github.com/hypersequent/oddk/internal/store/notifications"
)

type NotificationSender struct {
	store   *notifications.NotificationStore
	kvstore *store.Store
}

func NewNotificationSender(notifStore *notifications.NotificationStore, kvstore *store.Store) *NotificationSender {
	return &NotificationSender{
		store:   notifStore,
		kvstore: kvstore,
	}
}

// createNotifier creates a notifier and prepares the message body for the specific notification type
// Returns: (notifier, destination, preparedMessage, error)
func (s *NotificationSender) createNotifier(n *notifications.Notification, subject, body string) (notify.Notifier, string, string, error) {
	switch n.Type {
	case notifications.TypeEmail:
		return emailNotifier(n, subject, body)
	case notifications.TypeSlack:
		return slackNotifier(n, subject, body)
	case notifications.TypeTelegram:
		return telegramNotifier(n, subject, body)
	case notifications.TypeWebhook:
		return webhookNotifier(n, subject, body)
	default:
		return nil, "", "", fmt.Errorf("unsupported notification type: %s", n.Type)
	}
}

// combineSubjectBody joins subject and body as a single message; the subject
// is omitted when empty (used by channels without a separate subject field).
func combineSubjectBody(subject, body string) string {
	if subject == "" {
		return body
	}
	return fmt.Sprintf("%s\n\n%s", subject, body)
}

func emailNotifier(n *notifications.Notification, subject, body string) (notify.Notifier, string, string, error) {
	var cfg notifications.EmailConfig
	if err := json.Unmarshal(n.Config, &cfg); err != nil {
		return nil, "", "", fmt.Errorf("failed to parse email config: %w", err)
	}

	params := notify.SMTPParams{
		Host:               cfg.Host,
		Port:               cfg.Port,
		Username:           cfg.Username,
		Password:           cfg.Password,
		TLS:                cfg.TLS,
		StartTLS:           cfg.StartTLS,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}

	// Build the mailto destination string with from, to, and subject
	destination := fmt.Sprintf(`mailto:%s?from=%s`,
		strings.Join(cfg.To, ","), cfg.From)

	if subject != "" {
		destination = fmt.Sprintf("%s&subject=%s", destination, url.QueryEscape(subject))
	}

	// Email body is just the message body (subject is in URL)
	return notify.NewEmail(params), destination, body, nil
}

func slackNotifier(n *notifications.Notification, subject, body string) (notify.Notifier, string, string, error) {
	var cfg notifications.SlackConfig
	if err := json.Unmarshal(n.Config, &cfg); err != nil {
		return nil, "", "", fmt.Errorf("failed to parse slack config: %w", err)
	}

	// Slack message format: {"text": "message"}
	slackMessage := map[string]string{"text": combineSubjectBody(subject, body)}
	msgJSON, err := json.Marshal(slackMessage)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to marshal Slack message: %w", err)
	}

	return notify.NewWebhook(notify.WebhookParams{
		Headers: []string{"Content-Type:application/json"},
	}), cfg.SlackWebhookURL, string(msgJSON), nil
}

func telegramNotifier(n *notifications.Notification, subject, body string) (notify.Notifier, string, string, error) {
	var cfg notifications.TelegramConfig
	if err := json.Unmarshal(n.Config, &cfg); err != nil {
		return nil, "", "", fmt.Errorf("failed to parse telegram config: %w", err)
	}

	notifier, err := notify.NewTelegram(notify.TelegramParams{
		Token: cfg.Token,
	})
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to create telegram notifier: %w", err)
	}

	destination := fmt.Sprintf("telegram:%s", cfg.ChatID)
	return notifier, destination, combineSubjectBody(subject, body), nil
}

func webhookNotifier(n *notifications.Notification, subject, body string) (notify.Notifier, string, string, error) {
	var cfg notifications.WebhookConfig
	if err := json.Unmarshal(n.Config, &cfg); err != nil {
		return nil, "", "", fmt.Errorf("failed to parse webhook config: %w", err)
	}

	// Convert headers map to slice format
	var headers []string
	for k, v := range cfg.Headers {
		headers = append(headers, fmt.Sprintf("%s:%s", k, v))
	}

	preparedMessage, err := webhookMessage(&cfg, subject, body)
	if err != nil {
		return nil, "", "", err
	}

	return notify.NewWebhook(notify.WebhookParams{
		Headers: headers,
	}), cfg.URL, preparedMessage, nil
}

// webhookMessage renders the combined message in the webhook's configured
// request body format: plain text (default), JSON with the message under the
// configured key, or URL-encoded form data.
func webhookMessage(cfg *notifications.WebhookConfig, subject, body string) (string, error) {
	message := combineSubjectBody(subject, body)

	key := cfg.RequestBodyMessageKey
	if key == "" {
		key = "text"
	}

	switch cfg.RequestBodyType {
	case "plain", "":
		return message, nil

	case "json":
		msgJSON, err := json.Marshal(map[string]string{key: message})
		if err != nil {
			return "", fmt.Errorf("failed to marshal JSON message: %w", err)
		}
		return string(msgJSON), nil

	case "post":
		formData := url.Values{}
		formData.Set(key, message)
		return formData.Encode(), nil

	default:
		return "", fmt.Errorf("unsupported request body type: %s", cfg.RequestBodyType)
	}
}

func (s *NotificationSender) Send(ctx context.Context, name, subject, body string) error {
	notification, err := s.store.Get(name)
	if err != nil {
		return fmt.Errorf("failed to get notification: %w", err)
	}

	return s.sendNotification(ctx, notification, subject, body)
}

// sendNotification is the core method that handles sending a notification to a specific channel
func (s *NotificationSender) sendNotification(ctx context.Context, notification *notifications.Notification, subject, body string) error {
	notifier, destination, preparedMessage, err := s.createNotifier(notification, subject, body)
	if err != nil {
		errMsg := err.Error()
		_ = s.store.LogNotification(notification.Name, "error", nil, &errMsg)
		return err
	}

	// Email subject is already included in the destination URL by createNotifier

	fmt.Printf("Sending notification to %s (%s): destination='%s', message='%s'\n",
		notification.Name, notification.Type, destination, preparedMessage)

	err = notifier.Send(ctx, destination, preparedMessage)
	if err != nil {
		errMsg := err.Error()
		_ = s.store.LogNotification(notification.Name, "error", &subject, &errMsg)
		return fmt.Errorf("failed to send notification: %w", err)
	}

	_ = s.store.LogNotification(notification.Name, "success", &subject, nil)
	return nil
}

func (s *NotificationSender) SendToAll(ctx context.Context, subject, body string) error {
	notifications, err := s.store.List()
	if err != nil {
		return fmt.Errorf("failed to list notifications: %w", err)
	}

	if len(notifications) == 0 {
		return fmt.Errorf("no notifications configured")
	}

	var errors []string
	for _, n := range notifications {
		if err := s.sendNotification(ctx, &n, subject, body); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", n.Name, err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("some notifications failed: %s", strings.Join(errors, "; "))
	}

	return nil
}

func (s *NotificationSender) Test(ctx context.Context) error {
	displayName := s.kvstore.KV.GetDisplayName()
	subject := fmt.Sprintf("ODDK Test Notification (%s)", displayName)
	body := "This is a test notification from ODDK to verify your notification configuration is working correctly."

	return s.SendToAll(ctx, subject, body)
}
