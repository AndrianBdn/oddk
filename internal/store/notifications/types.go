package notifications

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/go-playground/validator/v10"

	"github.com/hypersequent/oddk/internal/rfc3339time"
)

type NotificationType string

const (
	TypeEmail    NotificationType = "email"
	TypeSlack    NotificationType = "slack"
	TypeTelegram NotificationType = "telegram"
	TypeWebhook  NotificationType = "webhook"
)

func ValidateNotificationType(t string) error {
	switch NotificationType(t) {
	case TypeEmail, TypeSlack, TypeTelegram, TypeWebhook:
		return nil
	default:
		return fmt.Errorf("invalid notification type: %s", t)
	}
}

type Notification struct {
	Name      string           `db:"name" json:"name"`
	Type      NotificationType `db:"type" json:"type"`
	Config    json.RawMessage  `db:"config" json:"config"`
	CreatedAt rfc3339time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt rfc3339time.Time `db:"updated_at" json:"updatedAt"`
}

type NotificationLog struct {
	ID               int              `db:"id" json:"id"`
	NotificationName string           `db:"notification_name" json:"notificationName"`
	Status           string           `db:"status" json:"status"`
	Message          *string          `db:"message" json:"message,omitempty"`
	Error            *string          `db:"error" json:"error,omitempty"`
	CreatedAt        rfc3339time.Time `db:"created_at" json:"createdAt"`
}

type EmailConfig struct {
	Host               string   `json:"host" validate:"required,hostname_rfc1123|ip"`
	Port               int      `json:"port" validate:"required,min=1,max=65535"`
	Username           string   `json:"username" validate:"required"`
	Password           string   `json:"password" validate:"required"`
	From               string   `json:"from" validate:"required,email"`
	To                 []string `json:"to" validate:"required,min=1,dive,email"`
	TLS                bool     `json:"tls"`                // Direct TLS connection
	StartTLS           bool     `json:"startTls"`           // Start with plain connection then upgrade to TLS
	InsecureSkipVerify bool     `json:"insecureSkipVerify"` // Skip certificate verification
}

type SlackConfig struct {
	SlackWebhookURL string `json:"slackWebhookUrl" validate:"required,url"`
}

type TelegramConfig struct {
	Token  string `json:"token" validate:"required,min=35"` // Telegram bot tokens vary in length
	ChatID string `json:"chatId" validate:"required"`       // Can be @username or numeric ID
}

type WebhookConfig struct {
	URL                   string            `json:"url" validate:"required,url"`
	Headers               map[string]string `json:"headers,omitempty"`
	RequestBodyType       string            `json:"requestBodyType,omitempty" validate:"omitempty,oneof=plain post json"` // plain, post (form-encoded), json
	RequestBodyMessageKey string            `json:"requestBodyMessageKey,omitempty"`                                      // key for message in POST/JSON body (default: "text")
}

var validate = validator.New()

// ValidateNotificationName validates notification name format
func ValidateNotificationName(name string) error {
	// Must start with letter, can contain alphanumeric, dash, underscore
	// Pattern: ^[a-zA-Z][a-zA-Z0-9_-]*$
	nameRegex := regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

	if name == "" {
		return fmt.Errorf("notification name cannot be empty")
	}

	if len(name) < 2 {
		return fmt.Errorf("notification name must be at least 2 characters long")
	}

	if len(name) > 50 {
		return fmt.Errorf("notification name must be at most 50 characters long")
	}

	if !nameRegex.MatchString(name) {
		return fmt.Errorf("notification name must start with a letter and contain only alphanumeric characters, dashes, and underscores")
	}

	return nil
}

// GetTemplate returns a pre-filled template for the given notification type
// The name parameter can include special prefixes like "slack-" to get specialized templates
func GetTemplate(notifType NotificationType, name string) (*Notification, error) {
	if err := ValidateNotificationType(string(notifType)); err != nil {
		return nil, err
	}

	var config any

	switch notifType {
	case TypeEmail:
		config = EmailConfig{
			Host:               "smtp.example.com",
			Port:               587,
			Username:           "your-email@example.com",
			Password:           "your-app-password",
			From:               "your-email@example.com",
			To:                 []string{"recipient@example.com"},
			TLS:                false,
			StartTLS:           true,
			InsecureSkipVerify: false,
		}
	case TypeSlack:
		config = SlackConfig{
			SlackWebhookURL: "https://hooks.slack.com/services/YOUR/WEBHOOK/URL",
		}
	case TypeTelegram:
		config = TelegramConfig{
			Token:  "1234567890:EXAMPLETELEGRAMTOKEN",
			ChatID: "@your_channel_or_chat_id",
		}
	case TypeWebhook:
		config = WebhookConfig{
			URL: "https://your-webhook.example.com/endpoint",
			Headers: map[string]string{
				"Content-Type":  "application/json",
				"Authorization": "Bearer your-token",
			},
			RequestBodyType:       "json",
			RequestBodyMessageKey: "text",
		}
	default:
		return nil, fmt.Errorf("unsupported notification type: %s", notifType)
	}

	configJSON, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal template config: %w", err)
	}

	return &Notification{
		Name:   name,
		Type:   notifType,
		Config: configJSON,
	}, nil
}

func ValidateConfig(notifType NotificationType, config json.RawMessage) error {
	switch notifType {
	case TypeEmail:
		var cfg EmailConfig
		if err := json.Unmarshal(config, &cfg); err != nil {
			return fmt.Errorf("invalid email config JSON: %w", err)
		}

		if err := validate.Struct(cfg); err != nil {
			return fmt.Errorf("email config validation failed: %w", err)
		}

		// Custom validation - TLS and StartTLS shouldn't both be true
		if cfg.TLS && cfg.StartTLS {
			return fmt.Errorf("email config cannot have both TLS and StartTLS enabled")
		}

		if strings.Contains(cfg.Host, "example.com") {
			return fmt.Errorf("email config cannot use example.com - please provide a real SMTP host")
		}

	case TypeSlack:
		var cfg SlackConfig
		if err := json.Unmarshal(config, &cfg); err != nil {
			return fmt.Errorf("invalid slack config JSON: %w", err)
		}

		if err := validate.Struct(cfg); err != nil {
			return fmt.Errorf("slack config validation failed: %w", err)
		}

		if strings.Contains(cfg.SlackWebhookURL, "YOUR/WEBHOOK/URL") {
			return fmt.Errorf("slack config cannot use example URL - please provide a real webhook URL")
		}

	case TypeTelegram:
		var cfg TelegramConfig
		if err := json.Unmarshal(config, &cfg); err != nil {
			return fmt.Errorf("invalid telegram config JSON: %w", err)
		}

		if err := validate.Struct(cfg); err != nil {
			return fmt.Errorf("telegram config validation failed: %w", err)
		}

		if strings.Contains(cfg.Token, "EXAMPLETELEGRAMTOKEN") {
			return fmt.Errorf("telegram config cannot use example token - please provide a real bot token")
		}

	case TypeWebhook:
		var cfg WebhookConfig
		if err := json.Unmarshal(config, &cfg); err != nil {
			return fmt.Errorf("invalid webhook config JSON: %w", err)
		}

		// Set default request body type if empty
		if cfg.RequestBodyType == "" {
			cfg.RequestBodyType = "plain"
		}

		// Set default message key if empty and using json/post
		if cfg.RequestBodyMessageKey == "" && (cfg.RequestBodyType == "json" || cfg.RequestBodyType == "post") {
			cfg.RequestBodyMessageKey = "text"
		}

		if err := validate.Struct(cfg); err != nil {
			return fmt.Errorf("webhook config validation failed: %w", err)
		}

		if strings.Contains(cfg.URL, "example.com") {
			return fmt.Errorf("webhook config cannot use example.com - please provide a real webhook URL")
		}
	}
	return nil
}
