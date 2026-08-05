package operations

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestNormalizeSnapshotFormat(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", SnapshotFormatPhysical, false}, // empty means the default
		{"physical", SnapshotFormatPhysical, false},
		{"logical", SnapshotFormatLogical, false},
		{"binary", "", true},
		{"Physical", "", true}, // no case folding: the wire value is exact
	}
	for _, c := range cases {
		got, err := NormalizeSnapshotFormat(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeSnapshotFormat(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("NormalizeSnapshotFormat(%q) = (%q, %v), want %q", c.in, got, err, c.want)
		}
	}
}

// Pre-0.1.61 archives predate the field, and every one of them is logical.
func TestEntryFormatTreatsEmptyAsLogical(t *testing.T) {
	if got := entryFormat(SnapshotInstanceEntry{}); got != SnapshotFormatLogical {
		t.Errorf("entryFormat(empty) = %q, want logical", got)
	}
	if got := entryFormat(SnapshotInstanceEntry{Format: "physical"}); got != SnapshotFormatPhysical {
		t.Errorf("entryFormat(physical) = %q, want physical", got)
	}
}

func TestSafeTarMemberName(t *testing.T) {
	for name, want := range map[string]string{
		".":            "",
		"./PG_VERSION": "PG_VERSION",
		"base/1/2":     "base/1/2",
		"a//b":         "a/b",
	} {
		got, err := safeTarMemberName(name)
		if err != nil || got != want {
			t.Errorf("safeTarMemberName(%q) = (%q, %v), want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"..", "../x", "a/../../x", "/etc/passwd"} {
		if got, err := safeTarMemberName(name); err == nil {
			t.Errorf("safeTarMemberName(%q) = %q, want refusal", name, got)
		}
	}
}

func TestStripFirstPathComponent(t *testing.T) {
	got, ok, err := stripFirstPathComponent("docker/PG_VERSION")
	if err != nil || !ok || got != "PG_VERSION" {
		t.Errorf("strip(docker/PG_VERSION) = (%q, %v, %v)", got, ok, err)
	}
	got, ok, err = stripFirstPathComponent("data/base/1/2")
	if err != nil || !ok || got != "base/1/2" {
		t.Errorf("strip(data/base/1/2) = (%q, %v, %v)", got, ok, err)
	}
	// The copied directory's own entry has nothing left after the strip.
	if _, ok, err := stripFirstPathComponent("docker"); ok || err != nil {
		t.Errorf("strip(docker) ok=%v err=%v, want the root entry skipped", ok, err)
	}
	if _, ok, err := stripFirstPathComponent("docker/"); ok || err != nil {
		t.Errorf("strip(docker/) ok=%v err=%v, want the root entry skipped", ok, err)
	}
	if _, _, err := stripFirstPathComponent("../escape"); err == nil {
		t.Error("strip(../escape) accepted, want refusal")
	}
}

// The exclusion list mirrors pg_basebackup's: runtime state goes, pg_wal stays
// (a cold copy has no separate WAL stream — recovery needs it).
func TestColdCopyExcluded(t *testing.T) {
	excluded := []string{
		"postmaster.pid",
		"postmaster.opts",
		"backup_label.old",
		"pg_replslot/some_slot",
		"pg_stat_tmp/global.stat",
		"pg_subtrans/0000",
		"pg_notify/0000",
		"base/1/pg_internal.init",
		"global/pg_internal.init",
	}
	for _, name := range excluded {
		if !coldCopyExcluded(name, false) {
			t.Errorf("coldCopyExcluded(%q) = false, want excluded", name)
		}
	}
	kept := []string{
		"PG_VERSION",
		"pg_wal/000000010000000000000001",
		"pg_wal/archive_status/000000010000000000000001.done",
		"base/1/2654",
		"global/pg_control",
		"postgresql.conf",
		"pg_hba.conf",
	}
	for _, name := range kept {
		if coldCopyExcluded(name, false) {
			t.Errorf("coldCopyExcluded(%q) = true, want kept", name)
		}
	}
	// The runtime-only DIRECTORIES themselves are kept: PostgreSQL expects
	// them to exist on start.
	for _, dir := range []string{"pg_replslot", "pg_stat_tmp", "pg_notify"} {
		if coldCopyExcluded(dir, true) {
			t.Errorf("coldCopyExcluded(%q, dir) = true, want the directory kept", dir)
		}
	}
}

func TestPGDataPrefix(t *testing.T) {
	if got, err := pgDataPrefix("/var/lib/postgresql/data", "/var/lib/postgresql/data"); err != nil || got != "" {
		t.Errorf("pg<=17 prefix = (%q, %v), want empty", got, err)
	}
	if got, err := pgDataPrefix("/var/lib/postgresql", "/var/lib/postgresql/18/docker"); err != nil || got != "18/docker" {
		t.Errorf("pg18 prefix = (%q, %v), want 18/docker", got, err)
	}
	if _, err := pgDataPrefix("/var/lib/postgresql", "/opt/pgdata"); err == nil {
		t.Error("PGDATA outside the volume accepted, want refusal")
	}
	if _, err := pgDataPrefix("/var/lib/postgresql", "/var/lib/postgresql-evil/data"); err == nil {
		t.Error("sibling-prefix PGDATA accepted, want refusal")
	}
}

// buildTar assembles an in-memory tar from (name, typeflag, body) triples.
func buildTar(t *testing.T, entries []struct {
	name string
	typ  byte
	body string
},
) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o600, Size: int64(len(e.body))}
		if e.typ == tar.TypeSymlink {
			hdr.Linkname = "/tmp/target"
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readTarNames(t *testing.T, r io.Reader) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	return names
}

func TestRewriteTarStreamPrefixesAndEmitsParents(t *testing.T) {
	src := buildTar(t, []struct {
		name string
		typ  byte
		body string
	}{
		{"PG_VERSION", tar.TypeReg, "18\n"},
		{"pg_wal", tar.TypeDir, ""},
		{"base/1/2654", tar.TypeReg, "data"},
	})

	var out bytes.Buffer
	if err := rewriteTarStream(tar.NewReader(bytes.NewReader(src)), &out, "18/docker", true, nil); err != nil {
		t.Fatal(err)
	}
	names := readTarNames(t, &out)
	want := []string{"18/", "18/docker/", "18/docker/PG_VERSION", "18/docker/pg_wal/", "18/docker/base/1/2654"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("rewritten members = %v, want %v", names, want)
	}
}

func TestRewriteTarStreamNoPrefix(t *testing.T) {
	src := buildTar(t, []struct {
		name string
		typ  byte
		body string
	}{
		{"PG_VERSION", tar.TypeReg, "17\n"},
	})
	var out bytes.Buffer
	if err := rewriteTarStream(tar.NewReader(bytes.NewReader(src)), &out, "", true, nil); err != nil {
		t.Fatal(err)
	}
	names := readTarNames(t, &out)
	if len(names) != 1 || names[0] != "PG_VERSION" {
		t.Errorf("members = %v, want [PG_VERSION] untouched", names)
	}
}

func TestRewriteTarStreamRefusesLinksAndEscapes(t *testing.T) {
	link := buildTar(t, []struct {
		name string
		typ  byte
		body string
	}{
		{"pg_tblspc/16400", tar.TypeSymlink, ""},
	})
	var out bytes.Buffer
	if err := rewriteTarStream(tar.NewReader(bytes.NewReader(link)), &out, "", false, nil); err == nil {
		t.Error("symlink member accepted, want refusal (tablespaces unsupported)")
	} else if !strings.Contains(err.Error(), "tablespace") {
		t.Errorf("symlink refusal should mention tablespaces, got: %v", err)
	}

	escape := buildTar(t, []struct {
		name string
		typ  byte
		body string
	}{
		{"../evil", tar.TypeReg, "x"},
	})
	out.Reset()
	if err := rewriteTarStream(tar.NewReader(bytes.NewReader(escape)), &out, "", false, nil); err == nil {
		t.Error("escaping member accepted, want refusal")
	}
}

// writeColdCopy must strip docker-cp's directory prefix, drop runtime state,
// and produce a zstd tar whose members sit at the data-directory root.
func TestWriteColdCopy(t *testing.T) {
	src := buildTar(t, []struct {
		name string
		typ  byte
		body string
	}{
		{"docker", tar.TypeDir, ""},
		{"docker/PG_VERSION", tar.TypeReg, "18\n"},
		{"docker/postmaster.pid", tar.TypeReg, "1\n"},
		{"docker/pg_replslot", tar.TypeDir, ""},
		{"docker/pg_replslot/slot/state", tar.TypeReg, "x"},
		{"docker/pg_wal", tar.TypeDir, ""},
		{"docker/pg_wal/000000010000000000000001", tar.TypeReg, "wal"},
		{"docker/base/1/pg_internal.init", tar.TypeReg, "cache"},
		{"docker/base/1/2654", tar.TypeReg, "data"},
	})

	var out bytes.Buffer
	if err := writeColdCopy(bytes.NewReader(src), &out); err != nil {
		t.Fatal(err)
	}
	zr, err := zstd.NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	names := readTarNames(t, zr)

	joined := strings.Join(names, ",")
	for _, want := range []string{"PG_VERSION", "pg_wal/000000010000000000000001", "base/1/2654", "pg_replslot/"} {
		if !strings.Contains(joined, want) {
			t.Errorf("cold copy is missing %q; members: %v", want, names)
		}
	}
	for _, banned := range []string{"postmaster.pid", "pg_replslot/slot", "pg_internal.init", "docker/"} {
		if strings.Contains(joined, banned) {
			t.Errorf("cold copy should not contain %q; members: %v", banned, names)
		}
	}
}

func TestWriteColdCopyRefusesLinks(t *testing.T) {
	src := buildTar(t, []struct {
		name string
		typ  byte
		body string
	}{
		{"docker/pg_tblspc/16400", tar.TypeSymlink, ""},
	})
	var out bytes.Buffer
	if err := writeColdCopy(bytes.NewReader(src), &out); err == nil {
		t.Error("cold copy accepted a symlink, want refusal (tablespaces unsupported)")
	}
}

func TestCheckSnapshotArch(t *testing.T) {
	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}

	physical := &SnapshotManifest{
		SourceArch: otherArch,
		Instances:  []SnapshotInstanceEntry{{Name: "a", HasData: true, Format: "physical"}},
	}
	if err := checkSnapshotArch(physical); err == nil {
		t.Error("cross-arch physical snapshot accepted, want refusal")
	}

	logical := &SnapshotManifest{
		SourceArch: otherArch,
		Instances:  []SnapshotInstanceEntry{{Name: "a", HasData: true, Format: "logical"}},
	}
	if err := checkSnapshotArch(logical); err != nil {
		t.Errorf("cross-arch logical snapshot refused: %v", err)
	}

	// Pre-0.1.61 archives have no SourceArch — and are all logical anyway, but
	// the arch check alone must not refuse them.
	legacy := &SnapshotManifest{
		Instances: []SnapshotInstanceEntry{{Name: "a", HasData: true, Format: "physical"}},
	}
	if err := checkSnapshotArch(legacy); err != nil {
		t.Errorf("empty SourceArch refused: %v", err)
	}

	sameArch := &SnapshotManifest{
		SourceArch: runtime.GOARCH,
		Instances:  []SnapshotInstanceEntry{{Name: "a", HasData: true, Format: "physical"}},
	}
	if err := checkSnapshotArch(sameArch); err != nil {
		t.Errorf("same-arch physical snapshot refused: %v", err)
	}
}

func TestBasebackupFailureHint(t *testing.T) {
	refusal := `pg_basebackup: error: connection to server at "127.0.0.1", port 5432 failed: FATAL:  no pg_hba.conf entry for replication connection from host "127.0.0.1", user "postgres", no encryption`
	if hint := basebackupFailureHint(refusal); !strings.Contains(hint, "--logical") {
		t.Errorf("replication refusal should hint at --logical, got %q", hint)
	}
	if hint := basebackupFailureHint("pg_basebackup: error: could not write to file: No space left on device"); hint != "" {
		t.Errorf("unrelated failure should carry no hint, got %q", hint)
	}
}

// Per-tablespace tars would be silently unrestorable, so capture must refuse
// them rather than catalogue a snapshot that cannot be applied.
func TestRefuseUnexpectedBasebackupFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"base.tar.zst", "pg_wal.tar", "backup_manifest"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := refuseUnexpectedBasebackupFiles(dir); err != nil {
		t.Errorf("clean basebackup dir refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "16400.tar.zst"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := refuseUnexpectedBasebackupFiles(dir)
	if err == nil {
		t.Fatal("tablespace tar accepted, want refusal")
	}
	if !strings.Contains(err.Error(), "tablespaces") || !strings.Contains(err.Error(), "16400.tar.zst") {
		t.Errorf("refusal should name the file and mention tablespaces, got: %v", err)
	}
}

func TestPGMajorFromPGVersion(t *testing.T) {
	for content, want := range map[string]int{"17\n": 17, "18": 18, "9.6\n": 9} {
		got, ok := pgMajorFromPGVersion(content)
		if !ok || got != want {
			t.Errorf("pgMajorFromPGVersion(%q) = (%d, %v), want %d", content, got, ok, want)
		}
	}
	for _, bad := range []string{"", "  \n", "beta", "-3"} {
		if got, ok := pgMajorFromPGVersion(bad); ok {
			t.Errorf("pgMajorFromPGVersion(%q) = %d, want not-ok", bad, got)
		}
	}
}

// The restore stream must capture PG_VERSION as it flows past — that is what
// lets restore refuse a data directory whose major contradicts instance.json
// before the container ever starts.
func TestRewriteTarStreamCapturesPGVersion(t *testing.T) {
	src := buildTar(t, []struct {
		name string
		typ  byte
		body string
	}{
		{"backup_label", tar.TypeReg, "START WAL LOCATION\n"},
		{"PG_VERSION", tar.TypeReg, "18\n"},
		{"base/1/PG_VERSION", tar.TypeReg, "99\n"}, // per-db copy must NOT win
	})
	var out bytes.Buffer
	var captured bytes.Buffer
	if err := rewriteTarStream(tar.NewReader(bytes.NewReader(src)), &out, "18/docker", true, &captured); err != nil {
		t.Fatal(err)
	}
	if captured.String() != "18\n" {
		t.Errorf("captured PG_VERSION = %q, want %q (root member only)", captured.String(), "18\n")
	}
	// The member itself must still have flowed through to the output intact.
	names := readTarNames(t, bytes.NewReader(out.Bytes()))
	if !strings.Contains(strings.Join(names, ","), "18/docker/PG_VERSION") {
		t.Errorf("PG_VERSION missing from rewritten stream: %v", names)
	}
}
