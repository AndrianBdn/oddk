package operations

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCreateRoleName(t *testing.T) {
	tests := []struct {
		line string
		want string
		ok   bool
	}{
		{"CREATE ROLE appuser;", "appuser", true},
		{"CREATE ROLE appuser", "appuser", true},
		{`CREATE ROLE "mixed-Case";`, "mixed-Case", true},
		{`CREATE ROLE "has space";`, "has space", true},
		{`CREATE ROLE "quote""inside";`, `quote"inside`, true},
		{"CREATE ROLE postgres;", "postgres", true},
		// pg_dumpall emits the ALTER separately; only CREATE declares a role.
		{"ALTER ROLE appuser WITH NOSUPERUSER INHERIT;", "", false},
		{"-- CREATE ROLE commented;", "", false},
		{"", "", false},
		{"CREATE ROLE ;", "", false},
		{`CREATE ROLE "unterminated;`, "", false},
		{"CREATE DATABASE app;", "", false},
	}

	for _, tc := range tests {
		got, ok := parseCreateRoleName(tc.line)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseCreateRoleName(%q) = (%q, %v), want (%q, %v)", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

// TestRoleNamesFromGlobals uses a realistic pg_dumpall -g excerpt. The
// exclusions matter: postgres and pg_* always exist on a fresh cluster, so
// expecting them would make verification fail on a perfectly good restore.
func TestRoleNamesFromGlobals(t *testing.T) {
	globals := `--
-- PostgreSQL database cluster dump
--

SET default_transaction_read_only = off;

--
-- Roles
--

CREATE ROLE appowner;
ALTER ROLE appowner WITH NOSUPERUSER INHERIT CREATEROLE NOCREATEDB LOGIN PASSWORD 'SCRAM-SHA-256$4096:abc$def';
CREATE ROLE "report-reader";
ALTER ROLE "report-reader" WITH NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB LOGIN PASSWORD 'SCRAM-SHA-256$4096:ghi$jkl';
CREATE ROLE postgres;
ALTER ROLE postgres WITH SUPERUSER INHERIT CREATEROLE CREATEDB LOGIN REPLICATION BYPASSRLS PASSWORD 'SCRAM-SHA-256$4096:mno$pqr';
CREATE ROLE pg_read_all_data;

--
-- Role memberships
--

GRANT appowner TO "report-reader" GRANTED BY postgres;
`
	dir := t.TempDir()
	path := filepath.Join(dir, "globals.sql")
	if err := os.WriteFile(path, []byte(globals), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := roleNamesFromGlobals(path)
	if err != nil {
		t.Fatalf("roleNamesFromGlobals: %v", err)
	}

	want := []string{"appowner", "report-reader"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("role[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRoleNamesFromGlobalsMissingFile(t *testing.T) {
	if _, err := roleNamesFromGlobals(filepath.Join(t.TempDir(), "absent.sql")); err == nil {
		t.Fatal("a missing globals.sql must be an error, not an empty role list")
	}
}

// TestIsVersionNewer pins the refusal rule: an older snapshot is fine (startup
// migrations handle it), a newer one must be refused.
func TestIsVersionNewer(t *testing.T) {
	tests := []struct {
		a, b        string
		wantNewer   bool
		wantCompare bool
	}{
		{"0.1.60", "0.1.52", true, true},
		{"0.1.52", "0.1.60", false, true},
		{"0.1.52", "0.1.52", false, true},
		{"0.2.0", "0.1.99", true, true},
		{"1.0.0", "0.9.9", true, true},
		{"v0.1.60", "0.1.52", true, true},
		{"0.1.60-rc1", "0.1.52", true, true},
		// Unparseable on either side: report "cannot compare" so the caller
		// skips the check rather than refusing on a string it cannot read.
		{"dev", "0.1.52", false, false},
		{"0.1.52", "unknown", false, false},
		{"0.1", "0.1.52", false, false},
	}

	for _, tc := range tests {
		newer, ok := isVersionNewer(tc.a, tc.b)
		if newer != tc.wantNewer || ok != tc.wantCompare {
			t.Errorf("isVersionNewer(%q, %q) = (%v, %v), want (%v, %v)",
				tc.a, tc.b, newer, ok, tc.wantNewer, tc.wantCompare)
		}
	}
}
