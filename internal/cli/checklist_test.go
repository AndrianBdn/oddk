package cli_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/cli"
)

// The fixture is a plausible daemon response: snapshot #7 is the newest
// completed snapshot, my-app and billing are in it with data, warehouse was
// captured configuration-only (it was stopped during a logical capture), and
// fresh was created after the capture. billing still has a legacy per-instance
// backup cron — an un-migrated schedule the audit must surface.
const checklistFixture = `{
	"generatedAt": "2026-07-08T12:00:00Z",
	"health": {
		"overall": "degraded",
		"checkedAt": "2026-07-08T11:59:00Z",
		"hostHealthy": true,
		"failDetails": "instance billing: connection refused"
	},
	"instances": [
		{
			"name": "my-app",
			"version": "17",
			"status": "running",
			"health": "ok",
			"parameterGroup": "default:2025-08-27",
			"snapshotCoverage": {
				"state": "covered",
				"snapshot": {"id": 7, "timestamp": "2026-07-08T02:30:00Z", "sizeBytes": 2147483648, "location": "local+s3"}
			}
		},
		{
			"name": "billing",
			"version": "17",
			"status": "running",
			"health": "failing",
			"parameterGroup": "default:2025-08-27",
			"legacyBackupCron": {"utcHour": 3, "cleanupLocalDays": 7, "cleanupRemoteDays": 14},
			"snapshotCoverage": {
				"state": "covered",
				"snapshot": {"id": 7, "timestamp": "2026-07-08T02:30:00Z", "sizeBytes": 2147483648, "location": "local+s3"}
			}
		},
		{
			"name": "warehouse",
			"version": "18",
			"status": "stopped",
			"health": "not-checked",
			"parameterGroup": "default:2025-08-27",
			"snapshotCoverage": {
				"state": "config-only",
				"snapshot": {"id": 7, "timestamp": "2026-07-08T02:30:00Z", "sizeBytes": 2147483648, "location": "local+s3"}
			}
		},
		{
			"name": "fresh",
			"version": "18",
			"status": "running",
			"health": "ok",
			"parameterGroup": "default:2025-08-27",
			"snapshotCoverage": {"state": "not-captured"}
		}
	],
	"snapshots": {
		"scheduled": true,
		"utcHour": 3,
		"intervalHours": 24,
		"format": "physical",
		"lastSnapshot": {"id": 7, "timestamp": "2026-07-08T02:30:00Z", "sizeBytes": 2147483648, "location": "local+s3"},
		"total": 3,
		"copies": {"localAndRemote": 2, "remoteOnly": 0, "localOnly": 1, "none": 0}
	},
	"notifications": {
		"configured": [
			{"name": "ops-mail", "type": "email"},
			{"name": "ops-slack", "type": "slack"}
		],
		"lastEvent": {
			"name": "ops-slack",
			"status": "success",
			"detail": "Health degraded",
			"createdAt": "2026-07-08T11:59:05Z"
		}
	}
}`

func newChecklistServer(t *testing.T, fixture string) (*httptest.Server, []string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/checklist" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": "not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture))
	}))
	env := []string{fmt.Sprintf("ODDK_CLI_CONFIG=%s", writeTestConfig(t, server.URL))}
	return server, env
}

func TestChecklistAction_BlockOutput(t *testing.T) {
	server, env := newChecklistServer(t, checklistFixture)
	defer server.Close()

	var buf bytes.Buffer
	if err := cli.Run([]string{"oddk", "checklist"}, env, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"Overall health: degraded (last check 2026-07-08T11:59:00Z)",
		"instance billing: connection refused",
		"Instances (4)",
		"my-app · PostgreSQL 17 · running",
		"default:2025-08-27",
		// Covered instances point at the snapshot that holds their data.
		"snapshot coverage #7 · 2026-07-08T02:30:00Z · local+s3",
		// A configuration-only capture must read as a problem, not protection.
		"configuration-only in #7 — NO data captured",
		// fresh post-dates the newest snapshot.
		"not yet captured (newer than the newest snapshot)",
		// billing's surviving per-instance schedule is an un-migrated to-do.
		"legacy backups",
		"daily backup cron at 03:00 UTC still scheduled — migrate: oddk snapshot migrate-from-backups",
		"scheduled: daily at 03:00 UTC, physical",
		"last snapshot: 2026-07-08T02:30:00Z (local+s3)",
		"stored: 3 · 2 local+s3, 1 local",
		"Notifications\n",
		"2 configured: ops-mail [email], ops-slack [slack]",
		"last event  2026-07-08T11:59:05Z · ops-slack · success — Health degraded",
		"last error  none",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}

	// my-app and billing are covered; warehouse (config-only) and fresh
	// (not-captured) must NOT render the covered line.
	if got := strings.Count(out, "snapshot coverage #7"); got != 2 {
		t.Errorf("expected exactly 2 covered instances, got %d:\n%s", got, out)
	}
	// Only billing still has a legacy schedule.
	if got := strings.Count(out, "legacy backups"); got != 1 {
		t.Errorf("expected exactly 1 legacy backup cron warning, got %d:\n%s", got, out)
	}
	// Backups are legacy and excluded from the audit — none of the retired
	// lines may resurface.
	for _, gone := range []string{"last good backup", "backups stored", "covered by scheduled snapshots"} {
		if strings.Contains(out, gone) {
			t.Errorf("retired backup line %q resurfaced in output:\n%s", gone, out)
		}
	}
}

func TestChecklistAction_NoSnapshots(t *testing.T) {
	// A deployment that has never taken a snapshot: every instance reads
	// "no completed snapshots" and the global section is the to-do.
	const fixture = `{
		"generatedAt": "2026-07-08T12:00:00Z",
		"health": {"overall": "unknown"},
		"instances": [
			{
				"name": "solo",
				"version": "17",
				"status": "running",
				"health": "unknown",
				"parameterGroup": "default:2025-08-27",
				"snapshotCoverage": {"state": "no-snapshots"}
			}
		],
		"snapshots": {"scheduled": false, "total": 0, "copies": {}},
		"notifications": {"configured": []}
	}`
	server, env := newChecklistServer(t, fixture)
	defer server.Close()

	var buf bytes.Buffer
	if err := cli.Run([]string{"oddk", "checklist"}, env, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"no completed snapshots",
		"not scheduled (oddk snapshot setup-cron --utc-hour <h>)",
		"last snapshot: never",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestChecklistAction_JSONOutput(t *testing.T) {
	server, env := newChecklistServer(t, checklistFixture)
	defer server.Close()

	var buf bytes.Buffer
	if err := cli.Run([]string{"oddk", "checklist", "--json"}, env, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, `"generatedAt": "2026-07-08T12:00:00Z"`) {
		t.Errorf("expected pretty-printed JSON, got:\n%s", out)
	}
	if strings.Contains(out, "PARAMETER GROUP") {
		t.Errorf("expected no table output in JSON mode, got:\n%s", out)
	}
}

func TestChecklistAction_DaemonError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "failed to build checklist: boom"}`))
	}))
	defer server.Close()

	env := []string{fmt.Sprintf("ODDK_CLI_CONFIG=%s", writeTestConfig(t, server.URL))}

	var buf bytes.Buffer
	err := cli.Run([]string{"oddk", "checklist"}, env, &buf)
	if err == nil {
		t.Fatal("expected error from daemon")
	}
	if !strings.Contains(err.Error(), "failed to build checklist") {
		t.Errorf("expected daemon error to surface, got: %v", err)
	}
}
