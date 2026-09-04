package operations

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The classifiers must see through the wrapping every caller adds and must
// decide on the SQLSTATE alone: the message is localized by lc_messages.
func TestPostgresErrorClassifiers(t *testing.T) {
	localized := func(code string) error {
		// The German text the server sends with lc_messages=de_DE, wrapped the
		// way ConnectToRunningInstance wraps it.
		return fmt.Errorf("failed to connect to PostgreSQL: %w",
			&pgconn.PgError{Severity: "FATAL", Code: code, Message: "Passwort-Authentifizierung für Benutzer »postgres« fehlgeschlagen"})
	}
	cases := []struct {
		name              string
		err               error
		auth, undefinedDB bool
	}{
		{"invalid password", localized("28P01"), true, false},
		{"no such role / no pg_hba line", localized("28000"), true, false},
		{"database does not exist", localized("3D000"), false, true},
		{"server starting up", localized("57P03"), false, false},
		{"not a server error", errors.New("dial tcp 10.88.0.1:5432: connection refused"), false, false},
		{"nil", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAuthFailure(tc.err); got != tc.auth {
				t.Errorf("isAuthFailure = %v, want %v", got, tc.auth)
			}
			if got := isUndefinedDatabase(tc.err); got != tc.undefinedDB {
				t.Errorf("isUndefinedDatabase = %v, want %v", got, tc.undefinedDB)
			}
		})
	}
}
