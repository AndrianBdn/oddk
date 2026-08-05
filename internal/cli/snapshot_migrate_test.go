package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/andrianbdn/oddk/internal/cli"
)

// fakeMigrateDaemon serves the four endpoints `snapshot migrate-from-backups`
// composes and records every request, so tests can assert on ORDER as well as
// effect — scheduling snapshots before retiring the backup schedules is the
// property that keeps an interrupted run safe.
type fakeMigrateDaemon struct {
	mu             sync.Mutex
	calls          []string
	backupPlansRaw string
	snapshotPlan   string // JSON for {"plan": ...}; "null" when unconfigured
	checklistRaw   string
	backupsRaw     string
	failDeleteFor  string
}

func (f *fakeMigrateDaemon) record(method, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+" "+path)
}

func (f *fakeMigrateDaemon) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeMigrateDaemon) start(t *testing.T) []string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == "GET" && r.URL.Path == "/api/cron/backup":
			_, _ = w.Write([]byte(f.backupPlansRaw))
		case r.Method == "GET" && r.URL.Path == "/api/cron/snapshot":
			_, _ = w.Write([]byte(`{"plan": ` + f.snapshotPlan + `}`))
		case r.Method == "GET" && r.URL.Path == "/api/checklist":
			_, _ = w.Write([]byte(f.checklistRaw))
		case r.Method == "GET" && r.URL.Path == "/api/backups":
			_, _ = w.Write([]byte(f.backupsRaw))
		case r.Method == "POST" && r.URL.Path == "/api/cron/snapshot":
			body := map[string]int{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = fmt.Fprintf(w,
				`{"utcHour": %d, "intervalHours": %d, "cleanupLocalDays": %d, "cleanupRemoteDays": %d}`,
				body["utcHour"], body["intervalHours"], body["cleanupLocalDays"], body["cleanupRemoteDays"])
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/cron/backup/"):
			if f.failDeleteFor != "" && strings.HasSuffix(r.URL.Path, "/"+f.failDeleteFor) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error": "boom"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status": "ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": "not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return []string{fmt.Sprintf("ODDK_CLI_CONFIG=%s", writeTestConfig(t, srv.URL))}
}

func defaultMigrateDaemon() *fakeMigrateDaemon {
	return &fakeMigrateDaemon{
		// Hours disagree (3, 3, 7) and retention windows differ.
		backupPlansRaw: `[
			{"instanceName": "app",     "utcHour": 3, "cleanupLocalDays": 7,  "cleanupRemoteDays": 14},
			{"instanceName": "billing", "utcHour": 7, "cleanupLocalDays": 10, "cleanupRemoteDays": 30},
			{"instanceName": "staging", "utcHour": 3, "cleanupLocalDays": 5,  "cleanupRemoteDays": 5}
		]`,
		snapshotPlan: "null",
		// The checklist no longer reports per-instance backups (they are
		// legacy); the size estimate is derived from GET /api/backups instead:
		// each instance's newest COMPLETED backup (app -> #3, billing -> #2),
		// so failed records and older duplicates must not inflate it.
		checklistRaw: `{"instances": [
			{"name": "app"}, {"name": "billing"}, {"name": "staging"}
		], "snapshots": {"scheduled": false}}`,
		// Estimate = app's #3 (newest completed, 1.5 GiB) + billing's #2
		// (0.5 GiB) = 2.0 GiB — NOT app's older #1, NOT the failed #4 despite
		// its newer timestamp, and NOT #5: "ghost" is a destroyed instance
		// (absent from the checklist), whose surviving backup records a
		// snapshot would not capture. Unmanaged local bytes = #1 + #2 =
		// 1.5 GiB (records with a local copy, any status, migrating plans only).
		backupsRaw: `[
			{"id": 1, "instanceName": "app",     "timestamp": "2026-07-01T03:00:00Z", "size": 1073741824, "status": "completed", "localLocation": "/b/1.tar.zst", "remoteLocation": "s3://x/1"},
			{"id": 2, "instanceName": "billing", "timestamp": "2026-07-08T03:00:00Z", "size": 536870912,  "status": "completed", "localLocation": "/b/2.tar.zst"},
			{"id": 3, "instanceName": "app",     "timestamp": "2026-07-08T03:00:00Z", "size": 1610612736, "status": "completed", "remoteLocation": "s3://x/3"},
			{"id": 4, "instanceName": "app",     "timestamp": "2026-07-09T03:00:00Z", "size": 999999999999, "status": "failed"},
			{"id": 5, "instanceName": "ghost",   "timestamp": "2026-07-09T03:00:00Z", "size": 999999999999, "status": "completed", "remoteLocation": "s3://x/5"}
		]`,
	}
}

func runMigrate(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := cli.Run(append([]string{"oddk", "snapshot", "migrate-from-backups"}, args...), env, &buf)
	return buf.String(), err
}

func TestMigrateFromBackups_DerivesPlanAndOrdersCalls(t *testing.T) {
	f := defaultMigrateDaemon()
	env := f.start(t)

	out, err := runMigrate(t, env, "--yes")
	if err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out)
	}

	// Most common hour (03, twice) wins over the outlier; retention is the
	// longest window anyone used, so nothing is shortened.
	for _, want := range []string{
		"Scheduled snapshot: daily at 03:00 UTC",
		"Keep local:  10 days",
		"Keep offsite: 30 days",
		"did not agree on an hour",
		"Removed 3 backup schedule(s)",
		// Estimate: each instance's newest COMPLETED backup from /api/backups
		// (app #3 1.5 GiB + billing #2 0.5 GiB), not older duplicates and not
		// the failed record — see defaultMigrateDaemon's backupsRaw.
		"x ~2.0 GiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}

	calls := f.recorded()
	postAt, firstDeleteAt := -1, -1
	for i, c := range calls {
		if c == "POST /api/cron/snapshot" && postAt < 0 {
			postAt = i
		}
		if strings.HasPrefix(c, "DELETE /api/cron/backup/") && firstDeleteAt < 0 {
			firstDeleteAt = i
		}
	}
	if postAt < 0 {
		t.Fatalf("snapshot schedule was never created: %v", calls)
	}
	if firstDeleteAt < 0 {
		t.Fatalf("backup schedules were never removed: %v", calls)
	}
	if postAt > firstDeleteAt {
		t.Errorf("snapshots must be scheduled BEFORE backup schedules are removed, so an "+
			"interrupted run leaves both rather than neither; got: %v", calls)
	}

	for _, name := range []string{"app", "billing", "staging"} {
		if !slices.Contains(calls, "DELETE /api/cron/backup/"+name) {
			t.Errorf("backup schedule for %s was not removed: %v", name, calls)
		}
	}
}

// Removing a backup schedule permanently ends age-based cleanup of that
// instance's existing archives. If this report goes quiet, deployments leak disk
// silently across a whole fleet.
func TestMigrateFromBackups_ReportsUnmanagedBackups(t *testing.T) {
	f := defaultMigrateDaemon()
	env := f.start(t)

	out, err := runMigrate(t, env, "--yes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"2 instance(s) still hold 2 local backup(s)",
		"1.5 GiB",
		"2 offsite copy(ies)",
		"NO LONGER PRUNED",
		"oddk backup remove-local",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestMigrateFromBackups_DryRunChangesNothing(t *testing.T) {
	f := defaultMigrateDaemon()
	env := f.start(t)

	out, err := runMigrate(t, env, "--dry-run")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Dry run: nothing was changed.") {
		t.Errorf("expected a dry-run notice, got:\n%s", out)
	}
	for _, c := range f.recorded() {
		if strings.HasPrefix(c, "POST") || strings.HasPrefix(c, "DELETE") {
			t.Errorf("dry run performed a write: %s", c)
		}
	}
}

func TestMigrateFromBackups_KeepsAnExistingSnapshotSchedule(t *testing.T) {
	f := defaultMigrateDaemon()
	f.snapshotPlan = `{"utcHour": 5, "intervalHours": 6, "cleanupLocalDays": 2, "cleanupRemoteDays": 90}`
	env := f.start(t)

	out, err := runMigrate(t, env, "--yes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "already exists and will be kept") {
		t.Errorf("expected the existing schedule to be preserved, got:\n%s", out)
	}
	if !strings.Contains(out, "every 6 hours, anchored at 05:00 UTC") {
		t.Errorf("expected the existing schedule to be shown, got:\n%s", out)
	}
	for _, c := range f.recorded() {
		if c == "POST /api/cron/snapshot" {
			t.Error("a deliberately-configured snapshot schedule must not be overwritten")
		}
	}
	if !strings.Contains(out, "Removed 3 backup schedule(s)") {
		t.Errorf("backup schedules should still be retired, got:\n%s", out)
	}
}

// Re-running across a fleet must be a quiet success on hosts already done.
func TestMigrateFromBackups_AlreadyMigratedIsQuietSuccess(t *testing.T) {
	f := defaultMigrateDaemon()
	f.backupPlansRaw = `[]`
	f.snapshotPlan = `{"utcHour": 3, "intervalHours": 24, "cleanupLocalDays": 7, "cleanupRemoteDays": 14}`
	env := f.start(t)

	out, err := runMigrate(t, env, "--yes")
	if err != nil {
		t.Fatalf("an already-migrated host must not error: %v", err)
	}
	if !strings.Contains(out, "Already migrated") {
		t.Errorf("expected an already-migrated notice, got:\n%s", out)
	}
	for _, c := range f.recorded() {
		if strings.HasPrefix(c, "POST") || strings.HasPrefix(c, "DELETE") {
			t.Errorf("already-migrated host was written to: %s", c)
		}
	}
}

// The already-migrated exit is the one a fleet rollout hits most often, so it
// must emit JSON like every other exit rather than human prose.
func TestMigrateFromBackups_AlreadyMigratedEmitsJSON(t *testing.T) {
	f := defaultMigrateDaemon()
	f.backupPlansRaw = `[]`
	f.snapshotPlan = `{"utcHour": 3, "intervalHours": 24, "cleanupLocalDays": 7, "cleanupRemoteDays": 14}`
	env := f.start(t)

	for _, args := range [][]string{{"--json", "--yes"}, {"--json", "--dry-run"}} {
		out, err := runMigrate(t, env, args...)
		if err != nil {
			t.Fatalf("%v: unexpected error: %v", args, err)
		}
		var report struct {
			KeptExistingPlan bool     `json:"keptExistingPlan"`
			RemovedSchedules []string `json:"removedSchedules"`
			SnapshotPlan     struct {
				UTCHour int `json:"utcHour"`
			} `json:"snapshotPlan"`
		}
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Errorf("%v produced non-JSON output (%v):\n%s", args, err, out)
			continue
		}
		if !report.KeptExistingPlan || report.SnapshotPlan.UTCHour != 3 {
			t.Errorf("%v: unexpected report: %+v", args, report)
		}
		// [] not null, so a consumer can treat both exits the same way.
		if report.RemovedSchedules == nil {
			t.Errorf("%v: removedSchedules should marshal as [], not null", args)
		}
	}
}

func TestMigrateFromBackups_NothingToMigrate(t *testing.T) {
	f := defaultMigrateDaemon()
	f.backupPlansRaw = `[]`
	env := f.start(t)

	_, err := runMigrate(t, env, "--yes")
	if err == nil {
		t.Fatal("expected an error when there is nothing to migrate and no snapshot schedule")
	}
	if !strings.Contains(err.Error(), "snapshot setup-cron") {
		t.Errorf("error should point at the direct command: %v", err)
	}
}

// An explicit override must beat "keep the existing schedule" — otherwise the
// preview would show a plan that is never written.
func TestMigrateFromBackups_OverrideBeatsExistingSchedule(t *testing.T) {
	f := defaultMigrateDaemon()
	f.snapshotPlan = `{"utcHour": 5, "intervalHours": 6, "cleanupLocalDays": 2, "cleanupRemoteDays": 90}`
	env := f.start(t)

	out, err := runMigrate(t, env, "--yes", "--utc-hour", "9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "with your overrides applied") {
		t.Errorf("expected the preview to say the existing plan was overridden, got:\n%s", out)
	}
	if !slices.Contains(f.recorded(), "POST /api/cron/snapshot") {
		t.Errorf("an explicit --utc-hour must be written, not silently dropped; calls: %v", f.recorded())
	}
	if !strings.Contains(out, "updated with your overrides") {
		t.Errorf("expected the apply step to report an update, got:\n%s", out)
	}
}

func TestMigrateFromBackups_Overrides(t *testing.T) {
	f := defaultMigrateDaemon()
	env := f.start(t)

	out, err := runMigrate(t, env, "--yes", "--utc-hour", "1", "--interval-hours", "6",
		"--cleanup-local-days", "3", "--cleanup-remote-days", "60")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"every 6 hours, anchored at 01:00 UTC",
		"Keep local:  3 days",
		"Keep offsite: 60 days",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestMigrateFromBackups_JSONRequiresYesOrDryRun(t *testing.T) {
	f := defaultMigrateDaemon()
	env := f.start(t)

	_, err := runMigrate(t, env, "--json")
	if err == nil {
		t.Fatal("expected --json alone to be refused")
	}
	if !strings.Contains(err.Error(), "--dry-run") {
		t.Errorf("error should name the alternatives: %v", err)
	}

	out, err := runMigrate(t, env, "--json", "--dry-run")
	if err != nil {
		t.Fatalf("--json --dry-run should work: %v", err)
	}
	var report struct {
		DryRun       bool `json:"dryRun"`
		SnapshotPlan struct {
			UTCHour          int `json:"utcHour"`
			CleanupLocalDays int `json:"cleanupLocalDays"`
		} `json:"snapshotPlan"`
		RemovedSchedules []string `json:"removedSchedules"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not valid JSON (%v):\n%s", err, out)
	}
	if !report.DryRun || report.SnapshotPlan.UTCHour != 3 || report.SnapshotPlan.CleanupLocalDays != 10 {
		t.Errorf("unexpected report: %+v", report)
	}
	if len(report.RemovedSchedules) != 3 {
		t.Errorf("expected 3 schedules listed for removal, got %v", report.RemovedSchedules)
	}
}

// A partial failure leaves both schedules active. That is safe, but not obvious,
// so the error has to say so and name what already happened.
func TestMigrateFromBackups_PartialFailureIsExplained(t *testing.T) {
	f := defaultMigrateDaemon()
	f.failDeleteFor = "billing"
	env := f.start(t)

	_, err := runMigrate(t, env, "--yes")
	if err == nil {
		t.Fatal("expected an error when a schedule removal fails")
	}
	for _, want := range []string{"over-protected", "billing", "backup setup-cron"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got: %v", want, err)
		}
	}
}
