package cli_test

import (
	"net/http"
	"strings"
	"testing"
)

func snapshotMake(t *testing.T, response string, args ...string) (string, error) {
	t.Helper()
	f := &fakeDaemon{handle: func(w http.ResponseWriter, r *http.Request) bool {
		_, _ = w.Write([]byte(response))
		return true
	}}
	return runCLI(t, f.start(t), append([]string{"snapshot", "make"}, args...)...)
}

func runSnapshotMake(t *testing.T, response string) string {
	t.Helper()
	out, err := snapshotMake(t, response)
	if err != nil {
		t.Fatalf("snapshot make: %v", err)
	}
	return out
}

// The per-instance list already names every instance, so "N with data, 0
// configuration-only" restates the total for the ordinary case where every
// instance was running.
func TestSnapshotMakeOutput_NoConfigOnlyBreakdown(t *testing.T) {
	out := runSnapshotMake(t, `{
		"id": 7, "path": "/var/lib/oddk/backups/snapshot-db01-20260731140312.tar.zst",
		"size": 1288490188, "timestamp": "2026-07-31T14:03:12Z",
		"instances": [{"name": "app", "version": "17", "image": "postgres:17", "hasData": true}],
		"instancesWithData": 1, "configOnly": 0
	}`)

	if !strings.Contains(out, "Instances: 1\n") {
		t.Errorf("expected a bare count when nothing is configuration-only, got:\n%s", out)
	}
	if strings.Contains(out, "configuration-only") {
		t.Errorf("should not mention configuration-only when there are none, got:\n%s", out)
	}
	// Same value as 'snapshot list' renders.
	if !strings.Contains(out, "Size: 1.2 GiB") {
		t.Errorf("expected a human-readable size, got:\n%s", out)
	}
	if strings.Contains(out, "bytes") {
		t.Errorf("raw byte count should not appear in the human output, got:\n%s", out)
	}
}

// The format is part of what the operator needs to see: it decides where the
// archive can be restored (physical = same major + same arch; logical =
// anywhere). A cold-captured instance is a real capture, but its restore comes
// back stopped, so the per-instance line must say so.
func TestSnapshotMakeOutput_FormatAndColdCapture(t *testing.T) {
	out := runSnapshotMake(t, `{
		"id": 9, "path": "/var/lib/oddk/backups/snapshot-db01-20260804090000.tar.zst",
		"size": 1048576, "timestamp": "2026-08-04T09:00:00Z",
		"format": "physical",
		"instances": [
			{"name": "app", "version": "18", "image": "postgres:18", "hasData": true, "format": "physical", "captureMode": "basebackup"},
			{"name": "warm", "version": "18", "image": "postgres:18", "hasData": true, "format": "physical", "captureMode": "cold"}
		],
		"instancesWithData": 2, "configOnly": 0
	}`)

	if !strings.Contains(out, "Format: physical (pg_basebackup)") {
		t.Errorf("expected the format line, got:\n%s", out)
	}
	if !strings.Contains(out, "captured cold") {
		t.Errorf("a cold capture must be visible per instance, got:\n%s", out)
	}
	// Both instances carry data, so the config-only warning must not appear.
	if strings.Contains(out, "configuration-only") {
		t.Errorf("cold captures are not configuration-only, got:\n%s", out)
	}
}

// When there IS a split it must be reported — a configuration-only instance
// comes back from a restore with no databases at all.
func TestSnapshotMakeOutput_ConfigOnlyReported(t *testing.T) {
	out := runSnapshotMake(t, `{
		"id": 8, "path": "/var/lib/oddk/backups/snapshot-db01-20260731140500.tar.zst",
		"size": 5368709120, "timestamp": "2026-07-31T14:05:00Z",
		"instances": [
			{"name": "app", "version": "17", "image": "postgres:17", "hasData": true},
			{"name": "old", "version": "16", "image": "postgres:16", "hasData": false,
			 "skipReason": "stopped - configuration only, no databases"}
		],
		"instancesWithData": 1, "configOnly": 1
	}`)

	if !strings.Contains(out, "Instances: 2 (1 with data, 1 configuration-only)") {
		t.Errorf("expected the breakdown when there is a split, got:\n%s", out)
	}
	if !strings.Contains(out, "hold NO database contents") {
		t.Errorf("the configuration-only warning must still appear, got:\n%s", out)
	}
	if !strings.Contains(out, "Size: 5.0 GiB") {
		t.Errorf("expected a human-readable size, got:\n%s", out)
	}
}

// An instance that could not be captured is a different problem from one that
// was not running, and must not be reported with the same shrug. The archive is
// real and was kept, but the command was asked to capture the deployment and did
// not — so it exits non-zero.
func TestSnapshotMakeOutput_CaptureFailureIsReportedAndFails(t *testing.T) {
	const response = `{
		"id": 11, "path": "/var/lib/oddk/backups/snapshot-db01-20260811090000.tar.zst",
		"size": 2097152, "timestamp": "2026-08-11T09:00:00Z",
		"format": "physical",
		"instances": [
			{"name": "app", "version": "17", "image": "postgres:17", "hasData": true, "format": "physical", "captureMode": "basebackup"},
			{"name": "billing", "version": "17", "image": "postgres:17", "hasData": false, "format": "physical",
			 "skipReason": "base backup FAILED (wal senders exhausted); databases not captured — this instance would restore EMPTY"},
			{"name": "old", "version": "16", "image": "postgres:16", "hasData": false,
			 "skipReason": "container is \"paused\", which is neither serving nor cleanly stopped; databases not captured"}
		],
		"instancesWithData": 1, "configOnly": 2,
		"captureFailures": [{"instance": "billing", "stage": "base backup", "error": "wal senders exhausted"}]
	}`

	out, err := snapshotMake(t, response)
	if err == nil {
		t.Fatal("a snapshot missing an instance's data exited zero — a script would read it as a complete capture")
	}
	if !strings.Contains(err.Error(), "billing") {
		t.Errorf("the error does not name the instance that failed: %v", err)
	}

	if !strings.Contains(out, "FAILED to be captured") || !strings.Contains(out, "wal senders exhausted") {
		t.Errorf("the capture failure and its cause must both be shown, got:\n%s", out)
	}
	// The two reasons an instance holds no data must not be conflated: one
	// instance was merely paused, one is broken.
	if !strings.Contains(out, "1 instance(s) were not running") {
		t.Errorf("the not-running count must exclude failed captures, got:\n%s", out)
	}
	if strings.Contains(out, "2 instance(s) were not running") {
		t.Errorf("a failed capture was counted as merely 'not running', got:\n%s", out)
	}
}

// --json is what a fleet script parses, so stdout must stay valid JSON while the
// exit code carries the failure.
func TestSnapshotMakeOutput_CaptureFailureFailsInJSONMode(t *testing.T) {
	const response = `{"id": 12, "path": "/x.tar.zst", "size": 10, "timestamp": "2026-08-11T09:00:00Z",
		"instances": [{"name": "billing", "hasData": false}], "instancesWithData": 0, "configOnly": 1,
		"captureFailures": [{"instance": "billing", "stage": "dump", "error": "connection refused"}]}`

	out, err := snapshotMake(t, response, "--json")
	if err == nil {
		t.Fatal("--json mode exited zero on an incomplete capture")
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("stdout is not the JSON body a script expects, got:\n%s", out)
	}
}
