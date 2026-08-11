package instances

// InstanceStatus is the lifecycle state of an instance, as stored in
// rdbms_instances.status.
//
// This type exists because the column used to be untyped free text with no
// CHECK, written as bare literals from a dozen sites — and it had quietly become
// a DATA-SAFETY input: snapshot capture, the health checker and every data
// command branch on it. Nothing owned the vocabulary, nothing validated it, and
// startup reconciliation repaired only the handful of values whoever wrote it
// happened to think of, so the rest survived every restart forever.
//
// Two rules keep that from coming back:
//   - Every status is classified here, in ONE place, as terminal, interrupted or
//     derived. Reconciliation consumes that classification instead of keeping its
//     own list (see decideReconcile), so adding a status without classifying it is
//     caught rather than silently ignored.
//   - The column carries a CHECK constraint (migration 020) listing exactly
//     these values, so a writer that bypasses this package fails loudly at the
//     database rather than storing something no reader understands.
type InstanceStatus string

const (
	// StatusRunning and StatusStopped are DERIVED: the container decides them,
	// and reconciliation aligns the stored row to what Docker actually reports.
	StatusRunning InstanceStatus = "running"
	StatusStopped InstanceStatus = "stopped"

	// StatusError is TERMINAL. It is never auto-cleared — only an explicit
	// operation promotes out of it. A container that happens to be up does not
	// prove the instance is well: "restoring" becomes "error" precisely because
	// its data may be incomplete while its server answers.
	StatusError InstanceStatus = "error"

	// The INTERRUPTED statuses are written while an operation is in flight.
	// Seeing one at startup can only mean that operation died, because
	// reconciliation runs before the executor accepts any work.
	StatusCreating      InstanceStatus = "creating"
	StatusRestoring     InstanceStatus = "restoring"
	StatusSwitching     InstanceStatus = "switching"
	StatusReconfiguring InstanceStatus = "reconfiguring"
	StatusUpgrading     InstanceStatus = "upgrading"

	// LEGACY, never written any more. ConsistencyCheckOp used to persist a
	// PostgreSQL probe verdict from an ordinary read command and had no path
	// back to "running", so these latched onto healthy instances and stayed
	// forever — invisible to health checks, which skip anything not "running".
	// They remain here only so rows written by an older ODDK still load and can
	// be un-latched by reconciliation. Once every deployment has restarted on
	// >= 0.1.68, drop them from legacyStatuses and from the migration-020 CHECK.
	StatusBroken     InstanceStatus = "broken"
	StatusBrokenPort InstanceStatus = "broken-port"
	StatusBrokenAuth InstanceStatus = "broken-auth"
)

// interruptedRemedies maps each interrupted status to what an operator has to do
// about it. The remedy is part of the classification because a status the daemon
// resets to "error" without saying why leaves the operator guessing.
var interruptedRemedies = map[InstanceStatus]string{
	StatusCreating:      "destroy and re-create it",
	StatusRestoring:     "its data is incomplete, re-apply the snapshot or restore it from a backup",
	StatusSwitching:     "the data volume is intact - 'instance start' it, then re-run the switch",
	StatusReconfiguring: "the data volume is intact - 'instance start' it, then re-apply the parameter group",
	StatusUpgrading:     "its data may be partially migrated, restore it from the pre-upgrade backup",
}

// derivedStatuses are decided by the container rather than the stored row.
//
// The legacy broken* family is here, not among the interrupted statuses,
// because it was never written by an operation. Deriving them from Docker is
// what un-latches an already-affected deployment on its next daemon restart.
var derivedStatuses = map[InstanceStatus]bool{
	StatusRunning:    true,
	StatusStopped:    true,
	StatusBroken:     true,
	StatusBrokenPort: true,
	StatusBrokenAuth: true,
}

// IsInterrupted reports whether this status means an operation died mid-flight,
// and returns the remedy for it.
func (s InstanceStatus) IsInterrupted() (remedy string, ok bool) {
	remedy, ok = interruptedRemedies[s]
	return remedy, ok
}

// IsDerived reports whether the container's actual state decides this status.
func (s InstanceStatus) IsDerived() bool { return derivedStatuses[s] }

// IsTerminal reports whether this status is a settled outcome that reconciliation
// must not change on its own.
func (s InstanceStatus) IsTerminal() bool {
	return s == StatusError || s == StatusRunning || s == StatusStopped
}

// Valid reports whether this is a status the codebase knows about. Anything else
// is a status someone added without classifying it, which is the mistake this
// package exists to make impossible to ignore.
func (s InstanceStatus) Valid() bool {
	if _, ok := interruptedRemedies[s]; ok {
		return true
	}
	return s == StatusError || derivedStatuses[s]
}

func (s InstanceStatus) String() string { return string(s) }

// AllStatuses returns every legal value, which is what the migration-020 CHECK
// constraint is built from — so the database and the Go vocabulary cannot drift.
func AllStatuses() []InstanceStatus {
	return []InstanceStatus{
		StatusRunning, StatusStopped, StatusError,
		StatusCreating, StatusRestoring, StatusSwitching, StatusReconfiguring, StatusUpgrading,
		StatusBroken, StatusBrokenPort, StatusBrokenAuth,
	}
}
