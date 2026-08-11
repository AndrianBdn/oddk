package operations

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A capture failure leaves partial output behind — a truncated base.tar.zst, a
// half-written dump directory. Archiving it would ship bytes no restore path
// reads (both branch on the manifest entry, which says hasData:false) and leave
// a trap for anyone who inspects the files and concludes the data is in there.
func TestResetToConfigOnly_DiscardsPartialCaptureOutput(t *testing.T) {
	dir := t.TempDir()

	mustWrite := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Everything a failed capture can have produced, in both formats.
	mustWrite(filepath.Join(snapshotBasebackupDir, "base.tar.zst"), "truncated")
	mustWrite(filepath.Join("databases", "app", "toc.dat"), "partial")
	mustWrite("globals.sql", "CREATE ROLE app;")
	mustWrite(databaseMetadataFile, "[]")
	mustWrite(instanceMetadataFile, `{"name":"app"}`)

	metaWrites := 0
	resetToConfigOnly(dir, func() error {
		metaWrites++
		return os.WriteFile(filepath.Join(dir, instanceMetadataFile), []byte(`{"name":"app"}`), 0o600)
	})

	if metaWrites != 1 {
		t.Errorf("instance.json was written %d times, want 1 — every entry must carry it, "+
			"including one whose capture failed, or apply cannot rebuild the container", metaWrites)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(left))
	for _, e := range left {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != instanceMetadataFile {
		t.Errorf("staging directory holds %v, want only %s", names, instanceMetadataFile)
	}
}

// The summary is what every layer above the capture reports: the cron phase
// error, the notification body, and the CLI's non-zero exit.
func TestCaptureFailureError(t *testing.T) {
	none := &MakeSnapshotResult{Instances: make([]SnapshotInstanceEntry, 2)}
	if err := none.CaptureFailureError(); err != nil {
		t.Errorf("a complete capture reported %v, want nil", err)
	}

	degraded := &MakeSnapshotResult{
		Instances: make([]SnapshotInstanceEntry, 3),
		CaptureFailures: []SnapshotCaptureFailure{
			{Instance: "billing", Stage: "base backup", Error: "wal senders exhausted"},
		},
	}
	err := degraded.CaptureFailureError()
	if err == nil {
		t.Fatal("a degraded capture reported success")
	}
	for _, want := range []string{"billing", "base backup", "wal senders exhausted", "1 of 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Three outcomes, not two. "The capture phase failed" used to mean "no archive
// exists"; a degraded snapshot writes one anyway, and telling an operator that
// nothing was produced when something was sends them to the wrong recovery.
func TestCronRunReporter_DistinguishesNothingFromPartial(t *testing.T) {
	cases := []struct {
		name            string
		archiveProduced bool
		wantPhrase      string
		notWantPhrase   string
	}{
		{
			name:          "no archive at all",
			wantPhrase:    "NO NEW ARCHIVE WAS PRODUCED",
			notWantPhrase: "AN ARCHIVE WAS PRODUCED",
		},
		{
			name:            "archive written but incomplete",
			archiveProduced: true,
			wantPhrase:      "AN ARCHIVE WAS PRODUCED",
			notWantPhrase:   "NO NEW ARCHIVE WAS PRODUCED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &cronRunReporter{label: "the whole deployment", what: "Snapshot"}
			if tc.archiveProduced {
				r.noteArchive()
			}
			r.record("backup", "fail", errors.New("instance billing could not be captured"))

			body := r.failureBody()
			if !strings.Contains(body, tc.wantPhrase) {
				t.Errorf("body does not say %q:\n%s", tc.wantPhrase, body)
			}
			if strings.Contains(body, tc.notWantPhrase) {
				t.Errorf("body wrongly says %q:\n%s", tc.notWantPhrase, body)
			}
			if !strings.Contains(body, "instance billing could not be captured") {
				t.Errorf("body drops the recorded cause:\n%s", body)
			}
		})
	}
}

// A shipping failure must keep saying that an archive exists — noteArchive is
// about the capture, not about the run being clean.
func TestCronRunReporter_UploadFailureIsNotACaptureFailure(t *testing.T) {
	r := &cronRunReporter{label: "the whole deployment", what: "Snapshot"}
	r.noteArchive()
	r.record("backup_upload", "fail", errors.New("connection reset"))

	body := r.failureBody()
	if strings.Contains(body, "ARCHIVE WAS PRODUCED") {
		t.Errorf("a failed upload triggered capture wording:\n%s", body)
	}
	if !strings.Contains(body, "offsite upload") {
		t.Errorf("body does not name the failed phase:\n%s", body)
	}
}
