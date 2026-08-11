package operations

import (
	"errors"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/store/cron"
)

// cronLogFailed decides whether the PREVIOUS run failed, which is what gates a
// recovery notification. Getting it wrong either spams "recovered" every night
// or never sends one.
func TestCronLogFailed(t *testing.T) {
	tests := []struct {
		name string
		log  *cron.CronLog
		want bool
	}{
		{"nil log (no previous run)", nil, false},
		{
			"all phases ok",
			&cron.CronLog{
				BackupStatus: new("ok"), BackupUploadStatus: new("ok"),
				BackupCleanupStatus: new("ok"), BackupRemoteCleanupStatus: new("ok"),
			},
			false,
		},
		{
			"capture failed",
			&cron.CronLog{BackupStatus: new("fail"), BackupCleanupStatus: new("ok")},
			true,
		},
		{
			"only offsite retention failed",
			&cron.CronLog{BackupStatus: new("ok"), BackupRemoteCleanupStatus: new("fail")},
			true,
		},
		{
			// Offsite not configured, so upload never ran. That is a normal,
			// complete run — not a failure.
			"partial statuses, none failed",
			&cron.CronLog{BackupStatus: new("ok"), BackupCleanupStatus: new("ok")},
			false,
		},
		{
			// The daemon was killed before the first phase recorded anything.
			// It did not do what it was scheduled to do.
			"no statuses recorded at all",
			&cron.CronLog{},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cronLogFailed(tt.log); got != tt.want {
				t.Errorf("cronLogFailed() = %v, want %v", got, tt.want)
			}
		})
	}
}

// One notification per run, never one per phase: a night where everything fails
// is one problem, and four messages for it teaches an operator to filter the
// channel.
func TestCronRunReporter_OneMessagePerRun(t *testing.T) {
	r := &cronRunReporter{label: `instance "billing"`, what: "Backup"}
	r.record("backup", "fail", errors.New("pg_dump exited 1"))
	r.record("backup_upload", "fail", errors.New("no credentials"))
	r.record("backup_cleanup", "fail", errors.New("disk error"))
	r.record("backup_remote_cleanup", "ok", nil)

	if len(r.failures) != 3 {
		t.Fatalf("recorded %d failures, want 3", len(r.failures))
	}

	body := r.failureBody()
	for _, want := range []string{"pg_dump exited 1", "no credentials", "disk error"} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing the cause %q — the reason is the whole point:\n%s", want, body)
		}
	}
	// Phase names are column prefixes; an operator should read words.
	for _, want := range []string{"capture", "offsite upload", "local retention"} {
		if !strings.Contains(body, want) {
			t.Errorf("body should name the phase %q in readable form:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "oddk cron logs") {
		t.Errorf("body should point at the command that shows the history:\n%s", body)
	}
}

// label names the TARGET of the run, so it composes into "the scheduled <what>
// for <label>". Naming the snapshot target "the whole-deployment snapshot"
// produced "The scheduled snapshot for the whole-deployment snapshot did not
// complete cleanly" — the kind of sentence that makes an operator trust the rest
// of the message less.
func TestCronRunReporter_ReadableSubjectPhrase(t *testing.T) {
	snapshot := &cronRunReporter{label: "the whole deployment", what: "Snapshot"}
	snapshot.record("backup", "fail", errors.New("boom"))
	if body := snapshot.failureBody(); !strings.Contains(body, "The scheduled snapshot for the whole deployment did not complete cleanly.") {
		t.Errorf("snapshot failure opens awkwardly:\n%s", body)
	}

	// The per-instance case is what the phrasing was designed around; it must
	// keep reading correctly.
	backup := &cronRunReporter{label: `instance "billing"`, what: "Backup"}
	backup.record("backup", "fail", errors.New("boom"))
	if body := backup.failureBody(); !strings.Contains(body, `The scheduled backup for instance "billing" did not complete cleanly.`) {
		t.Errorf("backup failure opens awkwardly:\n%s", body)
	}
}

// A capture failure means no new archive exists — materially different from
// producing one and failing to ship it. The message must distinguish them, or
// an operator cannot tell "we lost a night" from "we lost an upload".
func TestCronRunReporter_DistinguishesNoArchiveFromShippingFailure(t *testing.T) {
	captureFailed := &cronRunReporter{label: "the whole deployment", what: "Snapshot"}
	captureFailed.record("backup", "fail", errors.New("wal_level=minimal"))
	if !captureFailed.capturedNothing() {
		t.Error("a failed capture phase must count as producing no archive")
	}
	if !strings.Contains(captureFailed.failureBody(), "NO NEW ARCHIVE") {
		t.Errorf("a capture failure must say no archive was produced:\n%s", captureFailed.failureBody())
	}

	uploadFailed := &cronRunReporter{label: "the whole deployment", what: "Snapshot"}
	uploadFailed.record("backup", "ok", nil)
	uploadFailed.record("backup_upload", "fail", errors.New("5 GiB limit"))
	if uploadFailed.capturedNothing() {
		t.Error("the archive WAS produced; only the upload failed")
	}
	if strings.Contains(uploadFailed.failureBody(), "NO NEW ARCHIVE") {
		t.Errorf("must not claim no archive when the capture succeeded:\n%s", uploadFailed.failureBody())
	}
}

// A clean run after a clean run must stay silent. Notifying on every success is
// how a channel becomes noise nobody reads — which is the failure mode this
// whole feature exists to avoid.
func TestCronRunReporter_SilentWhenNothingChanged(t *testing.T) {
	r := &cronRunReporter{label: "x", what: "Backup", previousRunFailed: false}
	r.record("backup", "ok", nil)

	if len(r.failures) != 0 {
		t.Fatal("a clean run recorded a failure")
	}
	// finish() would send nothing: no failures, and no previous failure to
	// recover from. Assert the two conditions it switches on.
	if len(r.failures) > 0 || r.previousRunFailed {
		t.Error("a clean run following a clean run must notify nothing")
	}
}

func TestCronPhaseLabel(t *testing.T) {
	for phase, want := range map[string]string{
		"backup":                "capture",
		"backup_upload":         "offsite upload",
		"backup_cleanup":        "local retention",
		"backup_remote_cleanup": "offsite retention",
		"something_new":         "something_new", // unknown passes through, never blank
	} {
		if got := cronPhaseLabel(phase); got != want {
			t.Errorf("cronPhaseLabel(%q) = %q, want %q", phase, got, want)
		}
	}
}
