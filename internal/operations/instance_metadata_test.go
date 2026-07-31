package operations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/store/parameters"
)

func TestInstanceMetadataRoundTrip(t *testing.T) {
	dir := t.TempDir()

	want := &InstanceMeta{
		Name:           "billing",
		Version:        "17",
		Image:          "pgvector/pgvector:pg17-trixie",
		Port:           5433,
		CPUCores:       4,
		RAMMB:          8192,
		ParameterGroup: "custom-params",
		ParameterGroupDefinition: &parameters.ParameterGroup{
			Name: "custom-params",
			Parameters: []parameters.Parameter{
				{Name: "max_connections", Value: "200"},
				{Name: "shared_buffers", Value: "25%"},
			},
		},
	}

	if err := writeInstanceMetadata(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, found, err := readInstanceMetadata(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !found {
		t.Fatal("metadata not found after writing it")
	}

	if got.Name != want.Name || got.Version != want.Version || got.Image != want.Image {
		t.Errorf("identity mismatch: got %+v", got)
	}
	if got.Port != want.Port || got.CPUCores != want.CPUCores || got.RAMMB != want.RAMMB {
		t.Errorf("resources mismatch: got port=%d cpu=%d ram=%d", got.Port, got.CPUCores, got.RAMMB)
	}
	if got.ParameterGroup != want.ParameterGroup {
		t.Errorf("parameter group name = %q, want %q", got.ParameterGroup, want.ParameterGroup)
	}

	// The inlined definition is the point of the field: a target host has no
	// reason to hold a group by this name, so restore depends on the archive
	// carrying the parameters themselves.
	if got.ParameterGroupDefinition == nil {
		t.Fatal("parameter group definition was not preserved")
	}
	if len(got.ParameterGroupDefinition.Parameters) != 2 {
		t.Fatalf("got %d parameters, want 2", len(got.ParameterGroupDefinition.Parameters))
	}
	if got.ParameterGroupDefinition.Parameters[0].Value != "200" {
		t.Errorf("parameter value = %q, want %q",
			got.ParameterGroupDefinition.Parameters[0].Value, "200")
	}
}

// TestReadInstanceMetadataAbsent pins the backward-compatibility contract:
// archives predating instance.json must keep restoring exactly as they did, so
// a missing file is normal (found=false, nil error) rather than an error.
func TestReadInstanceMetadataAbsent(t *testing.T) {
	meta, found, err := readInstanceMetadata(t.TempDir())
	if err != nil {
		t.Fatalf("absent instance.json must not error, got: %v", err)
	}
	if found {
		t.Error("found = true for a directory with no instance.json")
	}
	if meta != nil {
		t.Errorf("meta = %+v, want nil", meta)
	}
}

// TestReadInstanceMetadataMalformed distinguishes "absent" from "corrupt": a
// present-but-unparseable file is a real error, not something to silently treat
// as an old archive.
func TestReadInstanceMetadataMalformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, instanceMetadataFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := readInstanceMetadata(dir); err == nil {
		t.Fatal("malformed instance.json must error")
	}
}

// TestInstanceMetadataOmitsSecrets guards the decision that no credential ever
// reaches instance.json. Per-instance archives are uploaded to S3 individually,
// so a password added to this struct would spread the instance's credential
// well beyond oddk.db.
func TestInstanceMetadataOmitsSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := writeInstanceMetadata(dir, &InstanceMeta{Name: "billing", Version: "17"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, instanceMetadataFile))
	if err != nil {
		t.Fatal(err)
	}

	for _, forbidden := range []string{"password", "Password", "3ncr.org"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("instance.json contains %q:\n%s", forbidden, data)
		}
	}
}
