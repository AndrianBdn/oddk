package operations

import (
	"errors"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/operr"
)

func TestRestoreDatabaseMembers(t *testing.T) {
	cases := []struct {
		name     string
		physical bool
		want     bool
	}{
		{"manifest.json", false, true},
		{"oddk.db", false, false}, // a logical restore never needs the source's store
		{"oddk.db", true, true},   // a physical one needs its credential
		{"instances", false, true},
		{"instances/", false, true},
		{"instances/app", false, true},
		{"instances/app/", false, true},
		{"instances/app/instance.json", false, true},
		{"instances/app/databases/sales/toc.dat", false, true},
		{"instances/app/basebackup/base.tar.zst", true, true},
		{"./instances/app/instance.json", false, true},
		{"instances/other/instance.json", false, false},
		{"instances/app2/instance.json", false, false}, // a name that merely starts the same
		{"instances/ap", false, false},
	}
	for _, c := range cases {
		if got := restoreDatabaseMembers("app", c.physical)(c.name); got != c.want {
			t.Errorf("keep(%q, physical=%v) = %v, want %v", c.name, c.physical, got, c.want)
		}
	}
}

func TestRefuseNewerSourceMajor(t *testing.T) {
	cases := []struct {
		source, target string
		refuse         bool
	}{
		{"17", "17", false},
		{"16", "17", false}, // pg_restore reads older dumps
		{"17.2", "18", false},
		{"18", "17", true},
		{"", "17", false}, // unknown: pg_restore stays the authority
		{"17", "nope", false},
	}
	for _, c := range cases {
		err := refuseNewerSourceMajor(c.source, c.target)
		if (err != nil) != c.refuse {
			t.Errorf("refuseNewerSourceMajor(%q, %q) = %v, want refuse=%v", c.source, c.target, err, c.refuse)
		}
		if err != nil && !errors.Is(err, operr.ErrInvalid) {
			t.Errorf("refusal is not tagged invalid (400): %v", err)
		}
	}
}

// psql prints the aggregated metadata as one JSON document; its shape must be
// DatabaseMeta's own, or databases.json — and with it the encoding/collation
// the restore recreates — silently comes out empty.
func TestParseScratchMetadata(t *testing.T) {
	raw := []byte(`[{"name":"sales","owner":"app","encoding":"UTF8","collate":"C.UTF-8","ctype":"C.UTF-8","locProvider":"c","createGrantees":["app","postgres"]}]` + "\n")
	metas, err := parseScratchMetadata(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("got %d metas", len(metas))
	}
	m := metas[0]
	if m.Name != "sales" || m.Owner != "app" || m.Encoding != "UTF8" || m.Collate != "C.UTF-8" ||
		m.Ctype != "C.UTF-8" || m.LocProvider != "c" || len(m.CreateGrantees) != 2 {
		t.Errorf("parsed %+v", m)
	}

	if metas, err := parseScratchMetadata([]byte("[]\n")); err != nil || len(metas) != 0 {
		t.Errorf("empty cluster: %v, %v", metas, err)
	}
	if _, err := parseScratchMetadata([]byte("psql: error: connection refused")); err == nil {
		t.Error("non-JSON output was accepted")
	}
}

func TestScratchMetadataSQLKeysMatchDatabaseMeta(t *testing.T) {
	sql := scratchMetadataSQL(17)
	for _, key := range []string{"'name'", "'owner'", "'encoding'", "'collate'", "'ctype'", "'locProvider'", "'createGrantees'"} {
		if !strings.Contains(sql, key) {
			t.Errorf("scratch metadata SQL does not emit %s", key)
		}
	}
}

// The source instance name is joined into the extraction filter and a staging
// path, so it is validated like any instance name — a "../" must never reach
// either.
func TestValidateRestoreDatabaseParams(t *testing.T) {
	ok := RestoreDatabaseParams{InstanceName: "staging", DatabaseName: "sales"}
	if err := validateRestoreDatabaseParams(&ok); err != nil {
		t.Fatalf("valid params refused: %v", err)
	}
	ok.SourceInstance = "prod"
	if err := validateRestoreDatabaseParams(&ok); err != nil {
		t.Fatalf("valid source instance refused: %v", err)
	}

	for _, bad := range []RestoreDatabaseParams{
		{InstanceName: "staging", SourceInstance: "../prod", DatabaseName: "sales"},
		{InstanceName: "staging", SourceInstance: "prod/x", DatabaseName: "sales"},
		{InstanceName: "", DatabaseName: "sales"},
		{InstanceName: "staging", DatabaseName: ""},
		{InstanceName: "staging", DatabaseName: "../sales"},
		{InstanceName: "staging", DatabaseName: "sales", RestoreAs: "a/b"},
	} {
		err := validateRestoreDatabaseParams(&bad)
		if err == nil {
			t.Errorf("accepted %+v", bad)
			continue
		}
		if !errors.Is(err, operr.ErrInvalid) {
			t.Errorf("refusal of %+v is not tagged invalid (400): %v", bad, err)
		}
	}
}

// event_triggers exists only from PostgreSQL 17 — which is also when LOGIN
// triggers appeared — and an older server REFUSES a connection that sends an
// unknown setting (verified on postgres:16: FATAL unrecognized configuration
// parameter). So it must be sent to 17+ and never below.
func TestScratchClientEnv(t *testing.T) {
	for major, wantEventTriggers := range map[int]bool{13: false, 16: false, 17: true, 18: true} {
		env := scratchClientEnv(major)
		if len(env) != 1 || !strings.HasPrefix(env[0], "PGOPTIONS=") {
			t.Fatalf("major %d: env = %v", major, env)
		}
		if got := strings.Contains(env[0], "event_triggers=off"); got != wantEventTriggers {
			t.Errorf("major %d: event_triggers=off present = %v, want %v (%s)", major, got, wantEventTriggers, env[0])
		}
		if !strings.Contains(env[0], "default_transaction_read_only=on") {
			t.Errorf("major %d: read-only missing: %s", major, env[0])
		}
	}
}
