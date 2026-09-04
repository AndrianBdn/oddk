package store_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/store"
	"github.com/andrianbdn/oddk/internal/store/notifications"
)

// Notification configs carry the credential for their channel: an SMTP
// password, a Slack webhook URL (the URL IS the credential), a Telegram bot
// token, an arbitrary webhook Authorization header. oddk.db is embedded
// verbatim in every snapshot archive, and that archive is NOT encrypted and is
// uploaded to S3 — so anything cleartext in this column is readable by anyone
// with read access to the bucket.

const smtpPassword = "s3cret-smtp-password"

func emailConfigJSON(t *testing.T) json.RawMessage {
	t.Helper()
	cfg, err := json.Marshal(notifications.EmailConfig{
		Host:     "smtp.mailhost.test",
		Port:     587,
		Username: "alerts@mailhost.test",
		Password: smtpPassword,
		From:     "alerts@mailhost.test",
		To:       []string{"ops@mailhost.test"},
		StartTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// rawConfigColumn reads the column as it actually sits on disk, bypassing the
// store. This is the assertion that proves the fix — everything else only
// proves the round-trip still works.
func rawConfigColumn(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	var stored string
	if err := st.Sqlx.QueryRow(`SELECT config FROM notifications WHERE name = ?`, name).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestNotificationConfigIsEncryptedAtRest(t *testing.T) {
	st := newTestStore(t)
	cfg := emailConfigJSON(t)

	if _, err := st.Notifications.Create("mail", notifications.TypeEmail, cfg); err != nil {
		t.Fatal(err)
	}

	stored := rawConfigColumn(t, st, "mail")
	if strings.Contains(stored, smtpPassword) {
		t.Fatalf("the SMTP password is in the clear on disk: %s", stored)
	}
	if strings.Contains(stored, "smtp.mailhost.test") {
		t.Fatalf("the config blob is not encrypted: %s", stored)
	}
	if !strings.HasPrefix(stored, crypto.ThreeNcrPrefix) {
		t.Fatalf("stored config should be 3ncr.org/1, got %q", stored)
	}

	// ...and it round-trips back to exactly what was written.
	got, err := st.Notifications.Get("mail")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Config) != string(cfg) {
		t.Fatalf("config did not round-trip:\n got %s\nwant %s", got.Config, cfg)
	}

	list, err := st.Notifications.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || string(list[0].Config) != string(cfg) {
		t.Fatalf("List did not decrypt: %+v", list)
	}
}

// A webhook's credential lives in an arbitrary header map, which is why the
// whole blob is encrypted rather than a list of known secret fields.
func TestWebhookAuthorizationHeaderIsEncryptedAtRest(t *testing.T) {
	st := newTestStore(t)

	cfg, err := json.Marshal(notifications.WebhookConfig{
		URL:                   "https://hooks.internal.test/oddk",
		Headers:               map[string]string{"Authorization": "Bearer super-secret-token"},
		RequestBodyType:       "json",
		RequestBodyMessageKey: "text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Notifications.Create("hook", notifications.TypeWebhook, cfg); err != nil {
		t.Fatal(err)
	}

	if stored := rawConfigColumn(t, st, "hook"); strings.Contains(stored, "super-secret-token") {
		t.Fatalf("webhook bearer token is in the clear on disk: %s", stored)
	}
}

func TestNotificationUpdateReEncrypts(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.Notifications.Create("mail", notifications.TypeEmail, emailConfigJSON(t)); err != nil {
		t.Fatal(err)
	}

	rotated, err := json.Marshal(notifications.TelegramConfig{
		Token:  "1234567890:AAHrotatedTELEGRAMtokenVALUE0000000",
		ChatID: "@ops",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Notifications.Update("mail", notifications.TypeTelegram, rotated)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Config) != string(rotated) {
		t.Fatalf("Update returned %s, want %s", got.Config, rotated)
	}

	stored := rawConfigColumn(t, st, "mail")
	if strings.Contains(stored, "rotatedTELEGRAMtoken") {
		t.Fatalf("rotated token is in the clear on disk: %s", stored)
	}
	if strings.Contains(stored, smtpPassword) {
		t.Fatal("the superseded SMTP password is still on disk")
	}
}

// Rows written by ODDK <= 0.1.79 hold cleartext JSON. They must keep working:
// the sweep converges storage, it is not what makes reads correct.
func TestCleartextConfigStillReads(t *testing.T) {
	st := newTestStore(t)
	cfg := emailConfigJSON(t)

	insertCleartextNotification(t, st, "legacy", cfg)

	got, err := st.Notifications.Get("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Config) != string(cfg) {
		t.Fatalf("cleartext config did not read back: %s", got.Config)
	}

	// The discriminator is unambiguous: a config blob is a JSON object, so it
	// can never begin with the 3ncr header.
	if strings.HasPrefix(string(cfg), crypto.ThreeNcrPrefix) {
		t.Fatal("a JSON config must never look like a 3ncr ciphertext")
	}
}

func TestEncryptStoredConfigsSweep(t *testing.T) {
	st := newTestStore(t)
	cfg := emailConfigJSON(t)

	insertCleartextNotification(t, st, "legacy", cfg)
	if _, err := st.Notifications.Create("current", notifications.TypeEmail, cfg); err != nil {
		t.Fatal(err)
	}

	var updatedBefore string
	if err := st.Sqlx.QueryRow(`SELECT updated_at FROM notifications WHERE name = 'legacy'`).Scan(&updatedBefore); err != nil {
		t.Fatal(err)
	}

	encrypted, errs := st.Notifications.EncryptStoredConfigs()
	if len(errs) != 0 {
		t.Fatalf("sweep reported errors: %v", errs)
	}
	if encrypted != 1 {
		t.Fatalf("sweep encrypted %d rows, want 1 (the already-encrypted row must be skipped)", encrypted)
	}

	stored := rawConfigColumn(t, st, "legacy")
	if !strings.HasPrefix(stored, crypto.ThreeNcrPrefix) || strings.Contains(stored, smtpPassword) {
		t.Fatalf("sweep did not encrypt the row: %s", stored)
	}
	got, err := st.Notifications.Get("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Config) != string(cfg) {
		t.Fatalf("sweep changed the config: %s", got.Config)
	}

	// The operator changed nothing, so the row must not claim an edit.
	var updatedAfter string
	if err := st.Sqlx.QueryRow(`SELECT updated_at FROM notifications WHERE name = 'legacy'`).Scan(&updatedAfter); err != nil {
		t.Fatal(err)
	}
	if updatedAfter != updatedBefore {
		t.Fatalf("sweep bumped updated_at: %s -> %s", updatedBefore, updatedAfter)
	}

	// Idempotent: it runs at every daemon start.
	again, errs := st.Notifications.EncryptStoredConfigs()
	if len(errs) != 0 || again != 0 {
		t.Fatalf("second sweep encrypted %d rows (errs %v), want 0", again, errs)
	}
}

// A mismatched master key must fail loudly rather than hand a garbled
// credential to a notifier — or silently skip the channel.
func TestNotificationConfigWithWrongMasterKeyFails(t *testing.T) {
	st, dir := newTestStoreDir(t)
	if _, err := st.Notifications.Create("mail", notifications.TypeEmail, emailConfigJSON(t)); err != nil {
		t.Fatal(err)
	}
	if err := st.Sqlx.Close(); err != nil {
		t.Fatal(err)
	}

	otherKey := make([]byte, crypto.KeyFileSize)
	for i := range otherKey {
		otherKey[i] = 0xAB
	}
	other, err := store.NewStore(filepath.Join(dir, "oddk.db"), dir, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Sqlx.Close() }()

	if _, err := other.Notifications.Get("mail"); err == nil {
		t.Fatal("reading a config with the wrong master key must fail")
	} else if !strings.Contains(err.Error(), "master key") {
		t.Fatalf("the error should name the cause, got: %v", err)
	}
	if _, err := other.Notifications.List(); err == nil {
		t.Fatal("List must fail too — a half-readable notification set is worse than none")
	}
}

func TestNewStoreRefusesAMasterKeyOfTheWrongSize(t *testing.T) {
	dir := t.TempDir()
	if _, err := store.NewStore(filepath.Join(dir, "oddk.db"), dir, []byte("too short")); err == nil {
		t.Fatal("NewStore must refuse a key it cannot encrypt with")
	}
}

// insertCleartextNotification writes the row the way every ODDK <= 0.1.79 did.
func insertCleartextNotification(t *testing.T, st *store.Store, name string, cfg json.RawMessage) {
	t.Helper()
	_, err := st.Sqlx.Exec(
		`INSERT INTO notifications (name, type, config, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		name, notifications.TypeEmail, string(cfg),
		"2026-01-02T03:04:05.000000000Z", "2026-01-02T03:04:05.000000000Z",
	)
	if err != nil {
		t.Fatal(err)
	}
}
