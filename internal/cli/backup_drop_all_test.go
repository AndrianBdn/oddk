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

// fakeDropAllDaemon serves the endpoints 'backup dangerously-drop-all'
// composes. The one that matters is DELETE /api/backups: a preview run must
// never issue it.
type fakeDropAllDaemon struct {
	mu           sync.Mutex
	calls        []string
	backupsRaw   string
	plansRaw     string
	checklistRaw string
	offsiteRaw   string
	deleteRaw    string
}

func (f *fakeDropAllDaemon) record(method, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+" "+path)
}

func (f *fakeDropAllDaemon) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeDropAllDaemon) start(t *testing.T) []string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/backups":
			_, _ = w.Write([]byte(f.backupsRaw))
		case r.Method == "GET" && r.URL.Path == "/api/cron/backup":
			_, _ = w.Write([]byte(f.plansRaw))
		case r.Method == "GET" && r.URL.Path == "/api/checklist":
			_, _ = w.Write([]byte(f.checklistRaw))
		case r.Method == "GET" && r.URL.Path == "/api/offsite":
			_, _ = w.Write([]byte(f.offsiteRaw))
		case r.Method == "DELETE" && r.URL.Path == "/api/backups":
			_, _ = w.Write([]byte(f.deleteRaw))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": "not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return []string{fmt.Sprintf("ODDK_CLI_CONFIG=%s", writeTestConfig(t, srv.URL))}
}

func defaultDropAllDaemon() *fakeDropAllDaemon {
	return &fakeDropAllDaemon{
		// "ghost" is destroyed (absent from the checklist) — its records are
		// the reason the sweep is a daemon endpoint at all.
		backupsRaw: `[
			{"id": 1, "instanceName": "app",   "timestamp": "2026-07-01T03:00:00Z", "size": 1073741824, "status": "completed", "localLocation": "/b/1.tar.zst", "remoteLocation": "s3://x/1"},
			{"id": 2, "instanceName": "app",   "timestamp": "2026-07-08T03:00:00Z", "size": 536870912,  "status": "completed", "localLocation": "/b/2.tar.zst"},
			{"id": 3, "instanceName": "ghost", "timestamp": "2026-07-09T03:00:00Z", "size": 100,        "status": "completed", "remoteLocation": "s3://x/3"}
		]`,
		plansRaw:     `[{"instanceName": "app", "utcHour": 3, "cleanupLocalDays": 7, "cleanupRemoteDays": 14}]`,
		checklistRaw: `{"instances": [{"name": "app"}], "snapshots": {"scheduled": false, "lastSnapshot": null}}`,
		offsiteRaw:   `{"active": false}`,
		deleteRaw: `{"recordsTotal": 3, "recordsDropped": 3, "recordsKept": 0,
			"localFilesDeleted": 2, "localFilesMissing": 0, "localBytesFreed": 1610612736,
			"remoteObjectsDeleted": 0, "remoteRefsDroppedNoOffsite": 2,
			"errors": [], "message": "Dropped 3 of 3 backup record(s)"}`,
	}
}

func runDropAll(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := cli.Run(append([]string{"oddk", "backup", "dangerously-drop-all"}, args...), env, &buf)
	return buf.String(), err
}

func TestDropAll_PreviewChangesNothing(t *testing.T) {
	f := defaultDropAllDaemon()
	env := f.start(t)

	out, err := runDropAll(t, env)
	if err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out)
	}

	for _, want := range []string{
		"DANGER",
		"3 record(s) across 2 instance(s)",
		"ghost (instance destroyed)",
		"offsite is NOT configured",
		"still have a backup schedule: [app]",
		"NO SNAPSHOT EXISTS",
		"Preview only — nothing was changed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview output missing %q\n%s", want, out)
		}
	}
	if slices.Contains(f.recorded(), "DELETE /api/backups") {
		t.Fatalf("preview must not delete anything; calls: %v", f.recorded())
	}
}

func TestDropAll_ApplyDeletesAndReports(t *testing.T) {
	f := defaultDropAllDaemon()
	env := f.start(t)

	out, err := runDropAll(t, env, "--apply", "--yes")
	if err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out)
	}
	if !slices.Contains(f.recorded(), "DELETE /api/backups") {
		t.Fatalf("apply must call DELETE /api/backups; calls: %v", f.recorded())
	}
	for _, want := range []string{
		"Dropped 3 of 3 backup record(s)",
		"Local archives deleted: 2",
		"Offsite references dropped (objects remain in the bucket): 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("apply output missing %q\n%s", want, out)
		}
	}
}

func TestDropAll_KeptRecordsFailTheCommand(t *testing.T) {
	f := defaultDropAllDaemon()
	f.deleteRaw = `{"recordsTotal": 3, "recordsDropped": 2, "recordsKept": 1,
		"localFilesDeleted": 1, "localBytesFreed": 100, "remoteObjectsDeleted": 0,
		"remoteRefsDroppedNoOffsite": 0,
		"errors": ["backup 1 (app): delete local file: permission denied"],
		"message": "Dropped 2 of 3 backup record(s); 1 kept because a copy could not be removed — fix the cause and re-run"}`
	env := f.start(t)

	out, err := runDropAll(t, env, "--apply", "--yes")
	if err == nil {
		t.Fatalf("kept records must make the command fail\n%s", out)
	}
	if !strings.Contains(out, "permission denied") {
		t.Errorf("output should list the per-record error\n%s", out)
	}
}

func TestDropAll_JSONPreview(t *testing.T) {
	f := defaultDropAllDaemon()
	env := f.start(t)

	out, err := runDropAll(t, env, "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out)
	}
	var report struct {
		DryRun               bool `json:"dryRun"`
		RecordsTotal         int  `json:"recordsTotal"`
		HasCompletedSnapshot bool `json:"hasCompletedSnapshot"`
		OffsiteConfigured    bool `json:"offsiteConfigured"`
		Instances            []struct {
			Name           string `json:"name"`
			InstanceExists bool   `json:"instanceExists"`
		} `json:"instances"`
		Result *json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if !report.DryRun || report.RecordsTotal != 3 || report.HasCompletedSnapshot ||
		report.OffsiteConfigured || report.Result != nil {
		t.Fatalf("unexpected report: %s", out)
	}
	if len(report.Instances) != 2 || report.Instances[1].Name != "ghost" || report.Instances[1].InstanceExists {
		t.Fatalf("instances misreported: %s", out)
	}
}

func TestDropAll_FlagValidation(t *testing.T) {
	f := defaultDropAllDaemon()
	env := f.start(t)

	if out, err := runDropAll(t, env, "--yes"); err == nil {
		t.Fatalf("--yes without --apply must fail\n%s", out)
	}
	if out, err := runDropAll(t, env, "--json", "--apply"); err == nil {
		t.Fatalf("--json --apply without --yes must fail\n%s", out)
	}
	// Neither validation failure may have touched the daemon destructively.
	if slices.Contains(f.recorded(), "DELETE /api/backups") {
		t.Fatalf("validation errors must not delete anything; calls: %v", f.recorded())
	}
}
