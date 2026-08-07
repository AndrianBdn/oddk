package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// hermeticAWSEnv pins the AWS default chain for the test process: only
// explicitly set env credentials resolve, the developer's real ~/.aws files
// and IMDS are out of the picture, and t.Setenv restores everything. Without
// this, a developer's live AWS session would leak into request-body
// assertions (both the CLI and this test share one process environment).
func hermeticAWSEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent-oddk-test")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent-oddk-test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
}

const restoreInstanceResponse = `{"instance":"app","created":false,"replaced":true,` +
	`"databases":1,"port":5432,"cpuCores":1,"ramMb":1024,"image":"postgres:17",` +
	`"passwordChanged":true,"format":"physical","finalStatus":"running"}`

// captureRestoreInstanceBody returns a fake daemon that records the decoded
// restore-instance request body.
func captureRestoreInstanceBody(body *map[string]any) *fakeDaemon {
	return &fakeDaemon{handle: func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && r.URL.Path == "/api/snapshot/restore-instance" {
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, body)
			_, _ = w.Write([]byte(restoreInstanceResponse))
			return true
		}
		return false
	}}
}

// TestRestoreInstanceByIDSendsCatalogueID pins the --id request shape: the
// catalogue id travels, no path travels, and — critically — no credentials
// travel: the by-id path is offsite-settings-only by definition.
func TestRestoreInstanceByIDSendsCatalogueID(t *testing.T) {
	hermeticAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIASHOULDNOTTRAVEL")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "should-not-travel")

	var body map[string]any
	fd := captureRestoreInstanceBody(&body)
	env := fd.start(t)

	out, err := runCLI(t, env, "snapshot", "restore-instance", "--instance", "app", "--id", "7", "--yes")
	if err != nil {
		t.Fatalf("restore-instance --id: %v\n%s", err, out)
	}
	if body["snapshotId"] != float64(7) {
		t.Errorf("snapshotId = %v, want 7", body["snapshotId"])
	}
	if _, ok := body["filePath"]; ok {
		t.Error("--id must not send filePath")
	}
	if _, ok := body["credentials"]; ok {
		t.Error("--id must never send credentials — that path is offsite-settings-only")
	}
}

// TestRestoreInstanceS3URISendsResolvedCreds pins the client→daemon credential
// transport: the shell's ambient triple (including the session token, which
// STS/SSO credentials are invalid without) and its resolved region land in the
// request body.
func TestRestoreInstanceS3URISendsResolvedCreds(t *testing.T) {
	hermeticAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_SESSION_TOKEN", "test-token")
	t.Setenv("AWS_REGION", "eu-central-1")

	var body map[string]any
	fd := captureRestoreInstanceBody(&body)
	env := fd.start(t)

	uri := "s3://foreign-bucket/oddk/*snapshots*/2026-08-01/snapshot-x-20260801000000.tar.zst"
	out, err := runCLI(t, env, "snapshot", "restore-instance",
		"--instance", "app", "--s3-uri", uri, "--master-key", "/keys/master.key", "--yes")
	if err != nil {
		t.Fatalf("restore-instance --s3-uri: %v\n%s", err, out)
	}

	if body["s3Uri"] != uri {
		t.Errorf("s3Uri = %v, want %q", body["s3Uri"], uri)
	}
	if body["region"] != "eu-central-1" {
		t.Errorf("region = %v, want the chain-resolved eu-central-1", body["region"])
	}
	if body["masterKeyPath"] != "/keys/master.key" {
		t.Errorf("masterKeyPath = %v", body["masterKeyPath"])
	}
	creds, ok := body["credentials"].(map[string]any)
	if !ok {
		t.Fatalf("credentials missing from body: %v", body)
	}
	if creds["accessKeyId"] != "AKIATEST" || creds["secretAccessKey"] != "test-secret" || creds["sessionToken"] != "test-token" {
		t.Errorf("unexpected credential triple: %v", creds)
	}
}

// TestRestoreInstanceS3URIWithoutShellCreds pins the empty-shell behavior:
// nothing is attached (the daemon's rung order decides — offsite settings or
// its own instance role) and the operator is told so.
func TestRestoreInstanceS3URIWithoutShellCreds(t *testing.T) {
	hermeticAWSEnv(t)

	var body map[string]any
	fd := captureRestoreInstanceBody(&body)
	env := fd.start(t)

	out, err := runCLI(t, env, "snapshot", "restore-instance",
		"--instance", "app", "--s3-uri", "s3://b/snap.tar.zst", "--yes")
	if err != nil {
		t.Fatalf("restore-instance --s3-uri (no creds): %v\n%s", err, out)
	}
	if _, ok := body["credentials"]; ok {
		t.Error("an empty shell must not attach credentials")
	}
	if !strings.Contains(out, "No AWS credentials in this shell") {
		t.Errorf("output should say the shell had no credentials:\n%s", out)
	}
}

// TestRestoreInstanceSourceFlagConflicts pins the exactly-one-of refusals and
// that they fire before any daemon call.
func TestRestoreInstanceSourceFlagConflicts(t *testing.T) {
	fd := &fakeDaemon{}
	env := fd.start(t)

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			"no source",
			[]string{"snapshot", "restore-instance", "--instance", "app", "--yes"},
			"exactly one of --file, --id or --s3-uri",
		},
		{
			"two sources",
			[]string{"snapshot", "restore-instance", "--instance", "app", "--file", "/x.tar.zst", "--id", "3", "--yes"},
			"exactly one of --file, --id or --s3-uri",
		},
		{
			"region without s3-uri",
			[]string{"snapshot", "restore-instance", "--instance", "app", "--file", "/x.tar.zst", "--region", "us-west-2", "--yes"},
			"only meaningful with --s3-uri",
		},
	}
	for _, tt := range tests {
		_, err := runCLI(t, env, tt.args...)
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.wantErr)
		}
	}
	if calls := fd.recorded(); len(calls) != 0 {
		t.Errorf("flag conflicts must refuse before any daemon call, got %v", calls)
	}
}

// TestSnapshotApplySourceFlagConflicts mirrors the same rule for apply, which
// runs daemon-less — the refusal happens before Docker, S3 or the data dir are
// touched.
func TestSnapshotApplySourceFlagConflicts(t *testing.T) {
	_, err := runCLI(t, nil, "snapshot", "apply", "--master-key", "/k")
	if err == nil || !strings.Contains(err.Error(), "exactly one of --file or --s3-uri") {
		t.Errorf("no source: err = %v", err)
	}
	_, err = runCLI(t, nil, "snapshot", "apply", "--master-key", "/k",
		"--file", "/x.tar.zst", "--s3-uri", "s3://b/x.tar.zst")
	if err == nil || !strings.Contains(err.Error(), "exactly one of --file or --s3-uri") {
		t.Errorf("two sources: err = %v", err)
	}
	_, err = runCLI(t, nil, "snapshot", "apply", "--master-key", "/k",
		"--file", "/x.tar.zst", "--region", "us-west-2")
	if err == nil || !strings.Contains(err.Error(), "only meaningful with --s3-uri") {
		t.Errorf("region without s3-uri: err = %v", err)
	}
}

// TestSnapshotListRemoteDaemonMode pins the zero-arg form: one GET to the
// daemon, a table with the objects newest-first, and the copy-paste hints that
// make the listing actionable.
func TestSnapshotListRemoteDaemonMode(t *testing.T) {
	listing := `{"bucket":"bkt","prefix":"oddk/*snapshots*/","truncated":false,"objects":[` +
		`{"uri":"s3://bkt/oddk/*snapshots*/2026-08-02/snapshot-db01-20260802030000.tar.zst",` +
		`"key":"oddk/*snapshots*/2026-08-02/snapshot-db01-20260802030000.tar.zst",` +
		`"size":2048,"lastModified":"2026-08-02T03:00:12Z"},` +
		`{"uri":"s3://bkt/oddk/*snapshots*/2026-08-01/snapshot-db01-20260801030000.tar.zst",` +
		`"key":"oddk/*snapshots*/2026-08-01/snapshot-db01-20260801030000.tar.zst",` +
		`"size":1024,"lastModified":"2026-08-01T03:00:12Z"}]}`

	fd := &fakeDaemon{handle: func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/api/snapshots/remote" {
			_, _ = w.Write([]byte(listing))
			return true
		}
		return false
	}}
	env := fd.start(t)

	out, err := runCLI(t, env, "snapshot", "list-remote")
	if err != nil {
		t.Fatalf("list-remote: %v\n%s", err, out)
	}
	for _, want := range []string{
		"LAST MODIFIED",
		"s3://bkt/oddk/*snapshots*/2026-08-02/snapshot-db01-20260802030000.tar.zst",
		"s3://bkt/oddk/*snapshots*/2026-08-01/snapshot-db01-20260801030000.tar.zst",
		"2.0 KiB",
		"Restore one instance:",
		"Rebuild a whole host:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if calls := fd.recorded(); len(calls) != 1 || calls[0] != "GET /api/snapshots/remote" {
		t.Errorf("calls = %v, want exactly one GET /api/snapshots/remote", calls)
	}
}

// TestSnapshotListRemoteURIModeNeedsNoCLIConfig pins the DR property: the
// direct s3:// form must run without any CLI config or token (a recovery host
// has neither), so it must get as far as the S3 attempt — never fail or nudge
// over daemon plumbing it does not use.
func TestSnapshotListRemoteURIModeNeedsNoCLIConfig(t *testing.T) {
	hermeticAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")

	// nil env: no ODDK_CLI_CONFIG anywhere. The endpoint refuses connections,
	// so reaching a dial error proves the S3 attempt happened.
	_, err := runCLI(t, nil, "snapshot", "list-remote", "s3://nowhere", "--endpoint", "http://127.0.0.1:1")
	if err == nil {
		t.Fatal("expected the unreachable endpoint to error")
	}
	if strings.Contains(err.Error(), "auth token") {
		t.Fatalf("URI-mode list-remote must not require a CLI config or token: %v", err)
	}
}

// TestSnapshotListRemoteDirectFlagsNeedURI pins the guard that the direct-mode
// flags are rejected in daemon mode, where they would be silently ignored.
func TestSnapshotListRemoteDirectFlagsNeedURI(t *testing.T) {
	fd := &fakeDaemon{}
	env := fd.start(t)
	_, err := runCLI(t, env, "snapshot", "list-remote", "--region", "us-west-2")
	if err == nil || !strings.Contains(err.Error(), "apply to the direct s3:// form") {
		t.Errorf("err = %v, want the direct-form explanation", err)
	}
	if calls := fd.recorded(); len(calls) != 0 {
		t.Errorf("must refuse before any daemon call, got %v", calls)
	}
}
