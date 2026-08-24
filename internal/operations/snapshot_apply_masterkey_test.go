package operations

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store/notifications"
)

// The point of the preflight is to prove the supplied --master-key belongs to
// the archive BEFORE apply installs it. Instance passwords are the usual
// evidence; a deployment with no instances used to have none at all, which
// stopped mattering once notification configs became encrypted with the same
// key — an empty deployment can still have channels configured, and a key that
// cannot read them installs a host that can never notify anyone.
func TestVerifyMasterKeyAgainstSnapshot_UsesNotificationsWhenThereAreNoInstances(t *testing.T) {
	st, dir := newTestStore(t)
	dbPath := filepath.Join(dir, "oddk.db")

	sourceKey, err := crypto.GetOrCreateKeyFile(dir)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := json.Marshal(notifications.SlackConfig{
		SlackWebhookURL: "https://hooks.slack.test/services/T000/B000/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Notifications.Create("slack", notifications.TypeSlack, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Sqlx.Close(); err != nil {
		t.Fatal(err)
	}

	if err := verifyMasterKeyAgainstSnapshot(dbPath, sourceKey); err != nil {
		t.Fatalf("the snapshot's own key must verify: %v", err)
	}

	wrongKey := make([]byte, crypto.KeyFileSize)
	for i := range wrongKey {
		wrongKey[i] = 0x5A
	}
	err = verifyMasterKeyAgainstSnapshot(dbPath, wrongKey)
	if err == nil {
		t.Fatal("a key that cannot read the snapshot's notification credentials must be refused")
	}
	if !errors.Is(err, operr.ErrInvalid) {
		t.Fatalf("the refusal should be a 400-class error, got %v", err)
	}
}

// Nothing encrypted at all: no instances, no notifications. There is nothing to
// verify against and nothing that could be bricked, so this must stay allowed —
// it is how a snapshot of a brand-new deployment applies.
func TestVerifyMasterKeyAgainstSnapshot_EmptyDeploymentIsAllowed(t *testing.T) {
	st, dir := newTestStore(t)
	if err := st.Sqlx.Close(); err != nil {
		t.Fatal(err)
	}

	anyKey := make([]byte, crypto.KeyFileSize)
	for i := range anyKey {
		anyKey[i] = 0x11
	}
	if err := verifyMasterKeyAgainstSnapshot(filepath.Join(dir, "oddk.db"), anyKey); err != nil {
		t.Fatalf("an empty deployment has nothing to verify against: %v", err)
	}
}
