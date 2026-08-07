package operations

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	s3service "github.com/andrianbdn/oddk/internal/services/s3"
	"github.com/andrianbdn/oddk/internal/store/offsite"
)

// TestPickSnapshotCredsSource pins the credential rung order for a daemon-side
// s3Uri fetch. The rules under test: offsite settings serve their OWN bucket
// only, and never against a substitute endpoint; request credentials beat the
// instance-role probe; the probe is the last resort.
func TestPickSnapshotCredsSource(t *testing.T) {
	endpoint := "https://minio.internal:9000"
	settingsPlain := &offsite.OffsiteSettings{Bucket: "own-bucket"}
	settingsWithEndpoint := &offsite.OffsiteSettings{Bucket: "own-bucket", Endpoint: &endpoint}

	tests := []struct {
		name        string
		settings    *offsite.OffsiteSettings
		bucket      string
		reqEndpoint string
		haveCreds   bool
		want        string
	}{
		{"no settings, request creds", nil, "any", "", true, credSourceRequest},
		{"no settings, no creds -> role probe", nil, "any", "", false, credSourceInstanceRole},
		{"own bucket -> offsite", settingsPlain, "own-bucket", "", false, credSourceOffsite},
		{"own bucket beats request creds", settingsPlain, "own-bucket", "", true, credSourceOffsite},
		{"foreign bucket, request creds", settingsPlain, "other-bucket", "", true, credSourceRequest},
		{"foreign bucket, no creds -> role probe", settingsPlain, "other-bucket", "", false, credSourceInstanceRole},
		// The endpoint-equality guard: offsite credentials must never be sent
		// to an endpoint other than the one they were configured for.
		{"own bucket, substitute endpoint", settingsPlain, "own-bucket", "https://evil.example:9000", true, credSourceRequest},
		{"own bucket, matching endpoint", settingsWithEndpoint, "own-bucket", endpoint, false, credSourceOffsite},
		{"own bucket, endpoint omitted in request", settingsWithEndpoint, "own-bucket", "", false, credSourceOffsite},
		{"own bucket, different endpoint", settingsWithEndpoint, "own-bucket", "https://other.example", true, credSourceRequest},
	}
	for _, tt := range tests {
		if got := pickSnapshotCredsSource(tt.settings, tt.bucket, tt.reqEndpoint, tt.haveCreds); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

// newFetchStubClient builds a client against an httptest S3 stub, using the
// NewClientFromConfig seam so no AWS environment is involved.
func newFetchStubClient(t *testing.T, handler http.HandlerFunc) *s3service.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test-key", "test-secret", ""),
	}
	return s3service.NewClientFromConfig(cfg, s3service.Target{Bucket: "b", Endpoint: srv.URL})
}

// TestFetchRemoteSnapshot covers the fetch lifecycle: atomic temp+rename, the
// provenance-checked reuse cache, refusal to reuse a same-basename impostor
// from a different URI, atomic replacement on change, and cleanup after a
// failed stream.
func TestFetchRemoteSnapshot(t *testing.T) {
	content := []byte("archive-bytes")
	failGet := false
	// Two keys share a basename and (initially) a size — the impostor case the
	// provenance sidecar exists to catch. The ETag varies with path+content,
	// like a real object store's would.
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/b/snap/x.tar.zst" && r.URL.Path != "/b/other/x.tar.zst" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			w.Header().Set("ETag", fmt.Sprintf("%q", r.URL.Path+fmt.Sprint(len(content))))
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if failGet {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(content)
		}
	}
	client := newFetchStubClient(t, handler)
	destDir := filepath.Join(t.TempDir(), "downloads")
	ctx := context.Background()

	// Fresh download: final file present with the right bytes, no temp left.
	res, err := FetchRemoteSnapshot(ctx, client, "s3://b/snap/x.tar.zst", "snap/x.tar.zst", destDir, nil)
	if err != nil {
		t.Fatalf("FetchRemoteSnapshot: %v", err)
	}
	if res.Reused {
		t.Error("first fetch must not report Reused")
	}
	got, err := os.ReadFile(res.Path)
	if err != nil || string(got) != string(content) {
		t.Fatalf("downloaded content = %q/%v", got, err)
	}
	assertNoTempFiles(t, destDir)

	// Same size at the same key: reused, not re-downloaded — and the mtime is
	// bumped so the TTL sweep cannot reap an archive mid-restore-series.
	old := time.Now().Add(-6 * 24 * time.Hour)
	if err := os.Chtimes(res.Path, old, old); err != nil {
		t.Fatal(err)
	}
	res2, err := FetchRemoteSnapshot(ctx, client, "s3://b/snap/x.tar.zst", "snap/x.tar.zst", destDir, nil)
	if err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
	if !res2.Reused {
		t.Error("matching-provenance fetch must report Reused")
	}
	if st, _ := os.Stat(res2.Path); time.Since(st.ModTime()) > time.Minute {
		t.Error("reuse must bump the cached archive's mtime")
	}

	// A DIFFERENT URI whose object shares the basename and the size must NOT
	// be served from the cache — the sidecar's URI/ETag mismatch forces a real
	// download. Reusing here would silently restore the wrong deployment.
	resOther, err := FetchRemoteSnapshot(ctx, client, "s3://b/other/x.tar.zst", "other/x.tar.zst", destDir, nil)
	if err != nil {
		t.Fatalf("impostor fetch: %v", err)
	}
	if resOther.Reused {
		t.Error("a same-basename same-size object from a different URI must not be reused")
	}

	// Different size: replaced atomically, no refusal — everything in the
	// downloads area is re-fetchable by construction.
	content = []byte("longer-archive-bytes-v2")
	res3, err := FetchRemoteSnapshot(ctx, client, "s3://b/snap/x.tar.zst", "snap/x.tar.zst", destDir, nil)
	if err != nil {
		t.Fatalf("replacing fetch: %v", err)
	}
	if res3.Reused {
		t.Error("size-mismatch fetch must not report Reused")
	}
	got, _ = os.ReadFile(res3.Path)
	if string(got) != string(content) {
		t.Errorf("replaced content = %q, want %q", got, content)
	}

	// A failed stream must leave neither a final file nor a temp partial —
	// while not touching the previously downloaded good copy... which a size
	// change plus failure WOULD replace, so remove it first for a clean check.
	if err := os.Remove(res3.Path); err != nil {
		t.Fatal(err)
	}
	failGet = true
	if _, err := FetchRemoteSnapshot(ctx, client, "s3://b/snap/x.tar.zst", "snap/x.tar.zst", destDir, nil); err == nil {
		t.Fatal("expected the failed stream to error")
	}
	if _, err := os.Stat(res3.Path); !os.IsNotExist(err) {
		t.Error("failed fetch must not leave a final file")
	}
	assertNoTempFiles(t, destDir)
}

func TestFetchRemoteSnapshotRefusals(t *testing.T) {
	client := newFetchStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	destDir := t.TempDir()

	// A key that is not an archive refuses before any network round-trip.
	_, err := FetchRemoteSnapshot(context.Background(), client, "s3://b/snap/readme.txt", "snap/readme.txt", destDir, nil)
	if err == nil || !strings.Contains(err.Error(), ".tar.zst") {
		t.Errorf("non-archive key: got %v, want a .tar.zst refusal", err)
	}

	// A missing object names the listing command that finds real ones.
	_, err = FetchRemoteSnapshot(context.Background(), client, "s3://b/snap/missing.tar.zst", "snap/missing.tar.zst", destDir, nil)
	if err == nil || !strings.Contains(err.Error(), "list-remote") {
		t.Errorf("missing object: got %v, want a list-remote hint", err)
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

// TestSweepSnapshotDownloads pins the pruning rules: finished archives age out
// after the TTL (mtime-based, so reuse keeps them), .tmp-* partials age out
// after an hour (a live download keeps its temp fresh), and a missing
// directory is a quiet no-op.
func TestSweepSnapshotDownloads(t *testing.T) {
	backupDir := t.TempDir()

	// No downloads directory at all: nothing to do, no error.
	if removed, err := SweepSnapshotDownloads(backupDir); err != nil || removed != 0 {
		t.Fatalf("missing dir sweep = %d/%v, want 0/nil", removed, err)
	}

	dir := SnapshotDownloadsDir(backupDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	mkFile := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}

	fresh := mkFile("snapshot-a-20260801000000.tar.zst", time.Hour)
	aged := mkFile("snapshot-b-20260701000000.tar.zst", 8*24*time.Hour)
	freshTmp := mkFile(".tmp-snapshot-c.tar.zst", 5*time.Minute)
	agedTmp := mkFile(".tmp-snapshot-d.tar.zst", 2*time.Hour)

	removed, err := SweepSnapshotDownloads(backupDir)
	if err != nil {
		t.Fatalf("SweepSnapshotDownloads: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	for p, wantGone := range map[string]bool{fresh: false, aged: true, freshTmp: false, agedTmp: true} {
		_, statErr := os.Stat(p)
		gone := os.IsNotExist(statErr)
		if gone != wantGone {
			t.Errorf("%s: gone=%v, want %v", filepath.Base(p), gone, wantGone)
		}
	}
	// The directory itself stays; delete/create churn buys nothing.
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("downloads dir must survive the sweep: %v", err)
	}
}
