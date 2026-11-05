package cli_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// verifyDaemon lists snapshots 1 to 4. Snapshot 2 is gone by the time it is
// verified (retention ran in between), snapshot 3 has a damaged copy, and the
// daemon fails on snapshot 4.
func verifyDaemon(t *testing.T) (*fakeDaemon, *atomic.Value, []string) {
	t.Helper()
	var lastQuery atomic.Value
	f := &fakeDaemon{handle: func(w http.ResponseWriter, r *http.Request) bool {
		lastQuery.Store(r.URL.RawQuery)
		switch r.Method + " " + r.URL.Path {
		case "GET /api/snapshots":
			_, _ = w.Write([]byte(`[{"id": 1}, {"id": 2}, {"id": 3}, {"id": 4}]`))
		case "POST /api/snapshot/1/verify":
			_, _ = w.Write([]byte(`{"id": 1, "filename": "a.tar.zst.age", "ok": true,
				"local": {"location": "/b/a.tar.zst.age", "status": "verified"}}`))
		case "POST /api/snapshot/3/verify":
			_, _ = w.Write([]byte(`{"id": 3, "filename": "c.tar.zst.age", "ok": false,
				"local": {"location": "/b/c.tar.zst.age", "status": "damaged", "detail": "archive is corrupt"}}`))
		case "POST /api/snapshot/4/verify":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error": "disk on fire"}`))
		default:
			return false // snapshot 2: the fake's JSON 404
		}
		return true
	}}
	return f, &lastQuery, f.start(t)
}

func TestSnapshotVerifyAll(t *testing.T) {
	_, _, env := verifyDaemon(t)
	out, err := runCLI(t, env, "snapshot", "verify", "--all")
	if err == nil {
		t.Fatalf("a damaged copy must make verify --all fail; output:\n%s", out)
	}
	// The snapshot removed mid-run is neither checked nor a failure.
	if !strings.Contains(err.Error(), "2 of 3 snapshot(s) failed verification") {
		t.Errorf("error = %v, want 2 of 3 (snapshot 2 skipped as removed)", err)
	}
	for _, want := range []string{"✓ local  verified", "Snapshot 2: removed since the listing", "✗ local  damaged", "archive is corrupt", "Snapshot 4:", "disk on fire", "2 snapshot(s) checked, 2 with problems"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// In --json mode a failed request is still an entry, so a script can tell
// which snapshot failed and why.
func TestSnapshotVerifyAllJSON(t *testing.T) {
	_, _, env := verifyDaemon(t)
	out, err := runCLI(t, env, "snapshot", "verify", "--all", "--json")
	if err == nil {
		t.Fatal("verify --all --json should fail")
	}
	var entries []struct {
		ID    int    `json:"id"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("stdout is not one JSON array: %v\n%s", err, out)
	}
	if len(entries) != 3 || entries[0].ID != 1 || !entries[0].OK || entries[1].ID != 3 || entries[1].OK ||
		entries[2].ID != 4 || entries[2].OK || !strings.Contains(entries[2].Error, "disk on fire") {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestSnapshotVerifyDeepAndJSON(t *testing.T) {
	f, query, env := verifyDaemon(t)
	out, err := runCLI(t, env, "snapshot", "verify", "1", "--deep", "--json")
	if err != nil {
		t.Fatalf("verify 1: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("a single id's --json output should be one object, got:\n%s", out)
	}
	calls := f.recorded()
	if len(calls) != 1 || calls[0] != "POST /api/snapshot/1/verify" {
		t.Errorf("calls = %v", calls)
	}
	if got, _ := query.Load().(string); got != "deep=true" {
		t.Errorf("--deep sent query %q, want deep=true", got)
	}

	// --all --json is always an array, even for an empty catalogue.
	empty := &fakeDaemon{handle: func(w http.ResponseWriter, r *http.Request) bool {
		_, _ = w.Write([]byte(`[]`))
		return true
	}}
	if out, err := runCLI(t, empty.start(t), "snapshot", "verify", "--all", "--json"); err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("empty catalogue: output %q, err %v; want []", out, err)
	}

	// A single unknown id is an error, not a skip.
	if _, err := runCLI(t, env, "snapshot", "verify", "2"); err == nil {
		t.Error("verify of a snapshot that does not exist should fail")
	}
	if _, err := runCLI(t, env, "snapshot", "verify", "1", "--all"); err == nil {
		t.Error("an id together with --all should be refused")
	}
}
