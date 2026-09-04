package notifications

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/andrianbdn/oddk/internal/crypto"
)

// The `config` blob is encrypted WHOLE, not field by field.
//
// Every channel keeps its credential in a different shape: an SMTP password, a
// Slack webhook URL that IS the credential, a Telegram bot token, and — for
// webhooks — an arbitrary `headers` map whose keys ODDK cannot enumerate, one
// of which is typically `Authorization`. A per-field scheme has to be extended
// for every new field, and the failure mode of forgetting one is SILENT: the
// secret is written in the clear and rides into every snapshot archive, which
// is unencrypted and uploaded to S3. Encrypting the blob cannot be defeated
// that way, and it covers the recipient addresses as a bonus.
//
// The cost is that the column is opaque to `sqlite3`. Only this package reads
// it, so nothing else has to care.

// ErrConfigUndecryptable marks a stored config that the master key cannot open,
// so callers can tell a key mismatch apart from a database error. `snapshot
// apply`'s preflight uses it to prove a supplied --master-key belongs to the
// archive even when the deployment has no instance to check against.
var ErrConfigUndecryptable = errors.New("notification config cannot be decrypted with this master key")

// encryptConfig turns a validated config blob into the value stored in the
// `config` column.
func encryptConfig(config json.RawMessage, masterKey []byte) (string, error) {
	encrypted, err := crypto.EncryptPassword(string(config), masterKey)
	if err != nil {
		return "", fmt.Errorf("encrypt notification config: %w", err)
	}
	return encrypted, nil
}

// decryptConfig turns a stored value back into a config blob, tolerating rows
// written before ODDK encrypted this column at all (<= 0.1.79).
//
// The discriminator is the 3ncr.org/1 header, and it is unambiguous: a config
// blob is a JSON object, so it starts with '{' (or whitespace) and can never
// start with "3ncr.org/1#".
//
// This is deliberately NOT crypto.DecryptPassword. That treats any non-3ncr
// value as the legacy base64+GCM ciphertext ODDK <= 0.1.28 wrote for
// PASSWORDS, and would try to base64-decode cleartext JSON. This column never
// had a legacy ciphertext format — it had no encryption at all, so "not 3ncr"
// means cleartext here and ciphertext there.
func decryptConfig(name, stored string, masterKey []byte) (json.RawMessage, error) {
	if !isEncryptedConfig(stored) {
		return json.RawMessage(stored), nil
	}
	plaintext, err := crypto.DecryptPassword(stored, masterKey)
	if err != nil {
		// Loud, and deliberately fatal to the whole read rather than a
		// skip-this-row: in every supported flow the key matches by
		// construction (`snapshot apply` installs the archive's own
		// master.key and refuses to proceed until it has proved the key
		// decrypts the archive's credentials). Reaching this means oddk.db
		// and master.key were paired by hand, which makes every channel
		// suspect — a notification system that silently drops one channel
		// and keeps delivering on the others hides exactly that.
		return nil, fmt.Errorf(
			"decrypt config of notification %q: %w: %w (restoring oddk.db by hand needs its matching master.key; "+
				"use `oddk snapshot apply`, which pairs them)",
			name, ErrConfigUndecryptable, err,
		)
	}
	return json.RawMessage(plaintext), nil
}

func isEncryptedConfig(stored string) bool {
	return strings.HasPrefix(stored, crypto.ThreeNcrPrefix)
}

// storedConfig is one row as it sits in the column, before decryption.
type storedConfig struct {
	name   string
	config string
}

// EncryptStoredConfigs encrypts any config still held in cleartext and returns
// how many rows it converted, plus one error per row it could not.
//
// This is the counterpart of the daemon's legacy-ciphertext sweep: rows written
// by ODDK <= 0.1.79 hold the config as plain JSON, and until they are rewritten
// every snapshot of this deployment ships those credentials in the clear.
// Reading works either way (decryptConfig tolerates cleartext), so this only
// converges storage — it is never load-bearing, and a failure must not stop the
// daemon.
//
// It is idempotent: an already-encrypted row is skipped, so it can run at every
// startup. `updated_at` is deliberately left alone — nothing about the
// operator's configuration changed, and bumping it would make `notify info`
// claim an edit that never happened.
func (s *NotificationStore) EncryptStoredConfigs() (int, []error) {
	rows, err := s.db.Query(`SELECT name, config FROM notifications ORDER BY name`)
	if err != nil {
		return 0, []error{fmt.Errorf("list notification configs: %w", err)}
	}

	// Read the whole result set BEFORE issuing any UPDATE. The store runs
	// SQLite with SetMaxOpenConns(1), so writing while a cursor is still open
	// would contend with itself on the single connection.
	var pending []storedConfig
	for rows.Next() {
		var row storedConfig
		if err := rows.Scan(&row.name, &row.config); err != nil {
			_ = rows.Close()
			return 0, []error{fmt.Errorf("scan notification config: %w", err)}
		}
		if isEncryptedConfig(row.config) {
			continue
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, []error{fmt.Errorf("read notification configs: %w", err)}
	}
	if err := rows.Close(); err != nil {
		return 0, []error{fmt.Errorf("close notification config cursor: %w", err)}
	}

	encrypted := 0
	var errs []error
	for _, row := range pending {
		ciphertext, err := encryptConfig(json.RawMessage(row.config), s.masterKey)
		if err != nil {
			errs = append(errs, fmt.Errorf("notification %q: %w", row.name, err))
			continue
		}
		if _, err := s.db.Exec(`UPDATE notifications SET config = ? WHERE name = ?`, ciphertext, row.name); err != nil {
			errs = append(errs, fmt.Errorf("store encrypted config of notification %q: %w", row.name, err))
			continue
		}
		encrypted++
	}
	return encrypted, errs
}
