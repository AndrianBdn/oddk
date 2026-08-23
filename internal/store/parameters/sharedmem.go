package parameters

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrSharedMemoryTooLarge is returned when the resolved parameter group would
// make PostgreSQL request a shared-memory arena larger than the container's
// RAM. Tagged so callers can map it to HTTP 400 before destroying anything.
var ErrSharedMemoryTooLarge = errors.New("estimated PostgreSQL shared memory exceeds container RAM")

const (
	// Documented PostgreSQL defaults, used when the group does not set them.
	defaultSharedBuffersBytes      = 128 * 1024 * 1024
	defaultMaxConnections          = 100
	defaultMaxLocksPerTransaction  = 64
	defaultMaxPreparedTransactions = 0
	defaultWalBuffersCapBytes      = 16 * 1024 * 1024

	// Per-lock cost of the lock table: sizeof(LOCK) + sizeof(PROCLOCK) plus
	// hash overhead, which the PostgreSQL docs put at roughly 270 bytes on
	// 64-bit. Deliberately NOT inflated "for safety": this estimate can only
	// REFUSE a group, and a refusal on the `snapshot apply` path is a disaster
	// recovery that will not proceed. Under-estimating is recoverable — the
	// container fails to start and applyClusterChange rolls back.
	lockObjectBytes = 270

	// Flat allowance for the arenas this does not model individually (proc
	// array, buffer descriptors, predicate locks, extension segments).
	sharedMemoryOverheadBytes = 64 * 1024 * 1024

	// pgBlockBytes is PostgreSQL's default block size. A memory GUC given as a
	// bare integer is counted in blocks, not bytes.
	pgBlockBytes = 8 * 1024

	ParameterTypePostgresCLIArg = "postgres_cli_arg"
)

// gucNameRe is a PostgreSQL GUC identifier, including custom class.field names.
var gucNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*(\.[a-z_][a-z0-9_]*)*$`)

// ValidateParameterIdentity checks the fields that PUT can enforce without
// knowing the target instance's RAM: name shape and that type is the only one
// CreateContainer/RecreateContainer actually pass through as postgres -c.
//
// This runs at PUT time only. It is deliberately NOT re-run when a container is
// built: groups stored before this validation existed must keep working, and a
// group that only ever produced a silently-ignored parameter must not become
// the reason a `snapshot apply` cannot rebuild the deployment.
func ValidateParameterIdentity(param Parameter, index int) error {
	label := fmt.Sprintf("parameter %d", index)
	if param.Name != "" {
		label = fmt.Sprintf("parameter %d (%s)", index, param.Name)
	}
	if param.Name == "" {
		return fmt.Errorf("%s: name is required", label)
	}
	if !gucNameRe.MatchString(param.Name) {
		return fmt.Errorf("%s: name is not a valid PostgreSQL GUC identifier", label)
	}
	if param.Type == "" {
		return fmt.Errorf("%s: type is required", label)
	}
	if param.Type != ParameterTypePostgresCLIArg {
		return fmt.Errorf("%s: type %q is not applied (only %s is passed as postgres -c); unknown types are refused rather than silently dropped",
			label, param.Type, ParameterTypePostgresCLIArg)
	}
	if param.ValueType == "" {
		return fmt.Errorf("%s: valueType is required", label)
	}
	if param.Value == "" {
		return fmt.Errorf("%s: value is required", label)
	}
	return nil
}

// ValidateForContainer estimates the shared-memory arena implied by a resolved
// group and refuses it when it cannot fit in ramMB. Call this at create/apply
// time with the instance's real RAM, not at PUT time (a group that is fatal on
// 8 GiB can be fine on 32 GiB).
//
// It refuses ONLY the case it can prove: an arena that does not fit. Anything
// it cannot parse leaves the estimate inexact and the group is allowed through,
// because the container-start path already catches a group PostgreSQL rejects
// and rolls back to the previous one. The asymmetry is deliberate — a false
// refusal here blocks `oddk snapshot apply`, which has no fallback.
func ValidateForContainer(resolved []ResolvedParameter, ramMB int) error {
	est, breakdown, exact := EstimateSharedMemoryBytes(resolved)
	if !exact {
		return nil
	}
	ramBytes := int64(ramMB) * 1024 * 1024
	if est > ramBytes {
		return fmt.Errorf("%w: %s, but the instance has %d MB RAM. Lower max_locks_per_transaction or max_connections, or use an instance with more RAM",
			ErrSharedMemoryTooLarge, breakdown, ramMB)
	}
	return nil
}

// EstimateSharedMemoryBytes returns a conservative estimate of the PostgreSQL
// shared-memory segment implied by resolved GUCs (shared_buffers + WAL buffers
// + lock table + a fixed overhead for other arenas). Unset GUCs use documented
// PostgreSQL defaults.
//
// exact reports whether every GUC it read parsed cleanly. When it is false the
// estimate is a guess and callers must not refuse on it. Parameters whose type
// is not postgres_cli_arg are skipped, matching the Docker layer, which only
// turns that type into `postgres -c name=value`.
func EstimateSharedMemoryBytes(resolved []ResolvedParameter) (est int64, breakdown string, exact bool) {
	byName := make(map[string]string, len(resolved))
	for _, p := range resolved {
		if p.Type != ParameterTypePostgresCLIArg {
			continue
		}
		byName[p.Name] = p.Value
	}
	exact = true

	sharedBuffers := int64(defaultSharedBuffersBytes)
	if v, ok := byName["shared_buffers"]; ok {
		if parsed, err := parsePGMemBytes(v); err == nil {
			sharedBuffers = parsed
		} else {
			exact = false
		}
	}

	maxConn := defaultMaxConnections
	if v, ok := byName["max_connections"]; ok {
		if parsed, err := parsePGInt(v); err == nil && parsed >= 0 {
			maxConn = parsed
		} else {
			exact = false
		}
	}

	maxLocks := defaultMaxLocksPerTransaction
	if v, ok := byName["max_locks_per_transaction"]; ok {
		if parsed, err := parsePGInt(v); err == nil && parsed >= 0 {
			maxLocks = parsed
		} else {
			exact = false
		}
	}

	maxPrepared := defaultMaxPreparedTransactions
	if v, ok := byName["max_prepared_transactions"]; ok {
		if parsed, err := parsePGInt(v); err == nil && parsed >= 0 {
			maxPrepared = parsed
		} else {
			exact = false
		}
	}

	walBuffers := min(sharedBuffers/32, int64(defaultWalBuffersCapBytes))
	if v, ok := byName["wal_buffers"]; ok {
		// -1 is the documented "derive from shared_buffers" sentinel, which is
		// what the block above already computed.
		if strings.TrimSpace(v) != "-1" {
			if parsed, err := parsePGMemBytes(v); err == nil {
				walBuffers = parsed
			} else {
				exact = false
			}
		}
	}

	lockTable := int64(maxLocks) * int64(maxConn+maxPrepared) * lockObjectBytes

	est = sharedBuffers + walBuffers + lockTable + sharedMemoryOverheadBytes
	breakdown = fmt.Sprintf("estimated %s PostgreSQL shared memory (shared_buffers %s + lock table %s from max_locks_per_transaction=%d × (max_connections=%d + max_prepared_transactions=%d))",
		formatBytes(est), formatBytes(sharedBuffers), formatBytes(lockTable), maxLocks, maxConn, maxPrepared)
	return est, breakdown, exact
}

func parsePGInt(value string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("not an integer: %q", value)
	}
	return n, nil
}

// pgMemUnits are PostgreSQL's memory suffixes, longest first so "kB" is not
// matched by a "B" test.
var pgMemUnits = []struct {
	suffix string
	factor int64
}{
	{"TB", 1024 * 1024 * 1024 * 1024},
	{"GB", 1024 * 1024 * 1024},
	{"MB", 1024 * 1024},
	{"KB", 1024},
	{"B", 1},
}

// parsePGMemBytes reads a PostgreSQL memory GUC. A bare integer is counted in
// 8 kB blocks, which is what PostgreSQL does for shared_buffers and
// wal_buffers — the two this estimator reads.
func parsePGMemBytes(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("memory value is empty")
	}
	upper := strings.ToUpper(value)
	for _, u := range pgMemUnits {
		if !strings.HasSuffix(upper, u.suffix) {
			continue
		}
		numPart := strings.TrimSpace(value[:len(value)-len(u.suffix)])
		n, err := strconv.ParseInt(numPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("memory value %q has invalid numeric part", value)
		}
		if n < 0 {
			return 0, fmt.Errorf("memory value %q is negative", value)
		}
		return n * u.factor, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("memory value %q is neither a block count nor a number with a B/kB/MB/GB/TB suffix", value)
	}
	if n < 0 {
		return 0, fmt.Errorf("memory value %q is negative", value)
	}
	return n * pgBlockBytes, nil
}

func formatBytes(b int64) string {
	if b >= 1024*1024*1024 {
		return fmt.Sprintf("%.1f GB", float64(b)/(1024*1024*1024))
	}
	return fmt.Sprintf("%d MB", (b+1024*1024-1)/(1024*1024))
}
