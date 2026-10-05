package cli_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const restoreDatabaseResponse = `{"instance":"app","sourceInstance":"app","sourceDatabase":"sales",` +
	`"targetDatabase":"sales_copy","format":"physical","sourceHost":"db01","snapshotAt":"2026-09-29T00:00:00Z"}`

func restoreFakeDaemon() *fakeDaemon {
	return &fakeDaemon{handle: func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/snapshot/restore-instance":
			_, _ = w.Write([]byte(restoreInstanceResponse))
			return true
		case r.Method == http.MethodPost && r.URL.Path == "/api/snapshot/restore-database":
			_, _ = w.Write([]byte(restoreDatabaseResponse))
			return true
		}
		return false
	}}
}

// With --json, stdout must be exactly one JSON document — no progress line,
// no credential note. A script that fails to parse it does so AFTER the
// restore happened, and a naive retry is then refused (restore-database: the
// database exists) or rebuilds the instance again (restore-instance).
//
// --s3-uri with an empty shell is used on purpose: it is the path that also
// prints the "No AWS credentials in this shell" note.
func TestRestoreJSONOutputIsOnlyJSON(t *testing.T) {
	for name, args := range map[string][]string{
		"restore-instance": {"snapshot", "restore-instance", "--instance", "app"},
		"restore-database": {"snapshot", "restore-database", "--instance", "app", "--database", "sales", "--restore-as", "sales_copy"},
	} {
		t.Run(name, func(t *testing.T) {
			hermeticAWSEnv(t)
			env := restoreFakeDaemon().start(t)

			out, err := runCLI(t, env, append(args, "--s3-uri", "s3://b/snap.tar.zst", "--yes", "--json")...)
			if err != nil {
				t.Fatalf("%s --json: %v\n%s", name, err, out)
			}
			dec := json.NewDecoder(strings.NewReader(out))
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				t.Fatalf("stdout is not JSON (%v):\n%s", err, out)
			}
			if dec.More() {
				t.Fatalf("stdout carries more than one JSON document:\n%s", out)
			}
			if strings.Contains(out, "Restoring") || strings.Contains(out, "AWS credentials") {
				t.Errorf("a progress line or note reached stdout:\n%s", out)
			}
		})
	}
}

// The confirmation prompt is interactive and writes to stdout, so --json
// without --yes is refused before anything is sent to the daemon.
func TestRestoreJSONRequiresYes(t *testing.T) {
	for name, args := range map[string][]string{
		"restore-instance": {"snapshot", "restore-instance", "--instance", "app", "--id", "7", "--json"},
		"restore-database": {"snapshot", "restore-database", "--instance", "app", "--database", "sales", "--id", "7", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			fd := restoreFakeDaemon()
			env := fd.start(t)
			_, err := runCLI(t, env, args...)
			if err == nil || !strings.Contains(err.Error(), "--json requires --yes") {
				t.Fatalf("want a --json requires --yes refusal, got %v", err)
			}
			if calls := fd.recorded(); len(calls) != 0 {
				t.Errorf("the daemon was called before the refusal: %v", calls)
			}
		})
	}
}
