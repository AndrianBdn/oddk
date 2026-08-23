package parameters_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/store/parameters"
)

func TestValidateParameterIdentity_RejectsUnknownType(t *testing.T) {
	err := parameters.ValidateParameterIdentity(parameters.Parameter{
		Name:      "shared_buffers",
		Type:      "something_else",
		ValueType: "numeric_mem",
		Value:     "128 MB",
	}, 0)
	if err == nil {
		t.Fatal("expected unknown type to be rejected")
	}
	if !strings.Contains(err.Error(), "something_else") || !strings.Contains(err.Error(), "postgres_cli_arg") {
		t.Fatalf("error should name the bad type and the only accepted one, got %v", err)
	}
}

func TestValidateParameterIdentity_RejectsBadGUCName(t *testing.T) {
	err := parameters.ValidateParameterIdentity(parameters.Parameter{
		Name:      "shared-buffers",
		Type:      parameters.ParameterTypePostgresCLIArg,
		ValueType: "numeric_mem",
		Value:     "128 MB",
	}, 0)
	if err == nil {
		t.Fatal("expected hyphenated name to be rejected")
	}
}

func TestValidateParameterIdentity_AcceptsCustomGUC(t *testing.T) {
	err := parameters.ValidateParameterIdentity(parameters.Parameter{
		Name:      "auto_explain.log_min_duration",
		Type:      parameters.ParameterTypePostgresCLIArg,
		ValueType: "numeric",
		Value:     "0",
	}, 0)
	if err != nil {
		t.Fatalf("custom GUC with a class prefix should be accepted: %v", err)
	}
}

func TestValidateForContainer_OutageShape(t *testing.T) {
	// The 2026-07-29 qasphere outage: max_locks_per_transaction=65536 with
	// max_connections=512 on an 8 GiB instance. Lock table alone is ~14 GB.
	resolved := []parameters.ResolvedParameter{
		{Name: "shared_buffers", Type: parameters.ParameterTypePostgresCLIArg, Value: "2 GB"},
		{Name: "max_connections", Type: parameters.ParameterTypePostgresCLIArg, Value: "512"},
		{Name: "max_locks_per_transaction", Type: parameters.ParameterTypePostgresCLIArg, Value: "65536"},
	}
	err := parameters.ValidateForContainer(resolved, 8*1024)
	if err == nil {
		t.Fatal("expected the outage-shaped group to be refused")
	}
	if !errors.Is(err, parameters.ErrSharedMemoryTooLarge) {
		t.Fatalf("want ErrSharedMemoryTooLarge, got %v", err)
	}
}

func TestValidateForContainer_DefaultGroupFits(t *testing.T) {
	resolved, err := parameters.ResolveParameters(parameters.GetDefaultParameters(), 4, 8*1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := parameters.ValidateForContainer(resolved, 8*1024); err != nil {
		t.Fatalf("default group on 8 GiB should fit: %v", err)
	}
}

func TestValidateForContainer_SameGroupFitsLargerRAM(t *testing.T) {
	// PUT must not refuse this: it is fatal on 8 GiB and fine on 32 GiB.
	resolved := []parameters.ResolvedParameter{
		{Name: "shared_buffers", Type: parameters.ParameterTypePostgresCLIArg, Value: "2 GB"},
		{Name: "max_connections", Type: parameters.ParameterTypePostgresCLIArg, Value: "512"},
		{Name: "max_locks_per_transaction", Type: parameters.ParameterTypePostgresCLIArg, Value: "65536"},
	}
	if err := parameters.ValidateForContainer(resolved, 32*1024); err != nil {
		t.Fatalf("outage-shaped group should fit 32 GiB: %v", err)
	}
}

func TestValidateForContainer_IgnoresNonCLIArgParameters(t *testing.T) {
	// The Docker layer only turns postgres_cli_arg into `postgres -c`, so a
	// group carrying anything else must still build a container. Refusing here
	// would brick `snapshot apply` for a deployment whose stored group predates
	// PUT-time type validation.
	resolved := []parameters.ResolvedParameter{
		{Name: "shared_buffers", Type: parameters.ParameterTypePostgresCLIArg, Value: "128 MB"},
		{Name: "some_legacy_knob", Type: "postgres_config_file", Value: "whatever"},
	}
	if err := parameters.ValidateForContainer(resolved, 1024); err != nil {
		t.Fatalf("a non-CLI-arg parameter must not refuse the container: %v", err)
	}
}

func TestValidateForContainer_UnparseableValueDoesNotRefuse(t *testing.T) {
	// An inexact estimate must never be the reason a container is refused: the
	// start+rollback path already catches a group PostgreSQL rejects, and a
	// false refusal on the DR path has no fallback.
	resolved := []parameters.ResolvedParameter{
		{Name: "shared_buffers", Type: parameters.ParameterTypePostgresCLIArg, Value: "not a size"},
		{Name: "max_locks_per_transaction", Type: parameters.ParameterTypePostgresCLIArg, Value: "65536"},
		{Name: "max_connections", Type: parameters.ParameterTypePostgresCLIArg, Value: "512"},
	}
	if err := parameters.ValidateForContainer(resolved, 1024); err != nil {
		t.Fatalf("an unparseable value must leave the estimate inexact, not refuse: %v", err)
	}
}

func TestEstimateSharedMemoryBytes_SuffixUnits(t *testing.T) {
	const (
		mib      = int64(1024 * 1024)
		overhead = int64(64) * mib
		// max_locks_per_transaction=64 × (max_connections=100 + prepared=0)
		defaultLockTable = int64(64 * 100 * 270)
	)
	for _, tc := range []struct {
		value         string
		sharedBuffers int64
	}{
		{"128MB", 128 * mib},
		{"128 MB", 128 * mib},
		{"2GB", 2048 * mib},
		{"131072kB", 128 * mib},
		{"134217728B", 128 * mib},
		{"16384", 128 * mib}, // bare integer = 8 kB blocks
	} {
		est, _, exact := parameters.EstimateSharedMemoryBytes([]parameters.ResolvedParameter{
			{Name: "shared_buffers", Type: parameters.ParameterTypePostgresCLIArg, Value: tc.value},
		})
		if !exact {
			t.Fatalf("%q should parse", tc.value)
		}
		walBuffers := min(tc.sharedBuffers/32, 16*mib)
		want := tc.sharedBuffers + walBuffers + defaultLockTable + overhead
		if est != want {
			t.Fatalf("%q: est = %d, want %d", tc.value, est, want)
		}
	}
}
