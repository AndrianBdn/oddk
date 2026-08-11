package operations

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
)

// Operation represents a unit of work that can be executed
type Operation interface {
	// Name returns a human-readable name for the operation
	Name() string

	Execute(ctx context.Context) error

	// Type returns whether this is a read or write operation
	Type() OpType
}

// OpType represents the type of operation
type OpType int

const (
	OpTypeRead OpType = iota
	OpTypeWrite
)

// Executor manages sequential execution of operations
type Executor struct {
	mu      sync.Mutex
	closing atomic.Bool
}

// NewExecutor creates a new operation executor
func NewExecutor() *Executor {
	return &Executor{}
}

// Execute runs an operation, ensuring sequential execution.
//
// Callers from HTTP handlers should pass context.Background(), not r.Context():
// operations are uninterruptible by HTTP client disconnect by design — a
// half-aborted pg_dump, pg_restore, S3 upload, or container-create would leave
// debris (orphan helper containers, partial files, inconsistent state). The ctx
// here is only useful if a caller intentionally wires in cancellation.
//
// Note that concurrent callers QUEUE on the mutex for the whole duration of
// whatever is running — they are not rejected. (An earlier "another operation
// is already running" fast-fail was unreachable dead code: the mutex is held
// for the operation's entire duration, so the flag it checked could never be
// set when the check ran.)
func (e *Executor) Execute(ctx context.Context, op Operation) error {
	if e.closing.Load() {
		return fmt.Errorf("daemon is shutting down, not starting operation %s", op.Name())
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Re-check after acquiring: we may have queued here for a long time, and
	// shutdown may have begun in the meantime.
	if e.closing.Load() {
		return fmt.Errorf("daemon is shutting down, not starting operation %s", op.Name())
	}

	log.Printf("Starting operation: %s", op.Name())

	err := op.Execute(ctx)
	if err != nil {
		log.Printf("Operation failed: %s - %v", op.Name(), err)
		return err
	}

	log.Printf("Operation completed: %s", op.Name())
	return nil
}

// ExecuteRead runs a read-only operation WITHOUT taking the serialization lock,
// so it answers while a long write operation is in flight.
//
// Why this exists: Execute holds one process-wide mutex for an operation's whole
// duration, and the read handlers took it too. During a whole-deployment
// snapshot — minutes to hours — `oddk list`, `oddk backup list` and
// `oddk checklist` blocked on that mutex and then died at the HTTP WriteTimeout
// with a bare EOF. The commands an operator reaches for to find out what is
// happening were exactly the ones that stopped answering while something was
// happening. OpType has been declared on every operation since the beginning and
// consulted nowhere; this is what it was for.
//
// The safety argument has three parts, and all three must hold for an operation
// to be routed here:
//
//  1. It must not write. Not to SQLite, not to Docker, not to disk. A read that
//     mutates could interleave with the write operation it is running alongside.
//     ConsistencyCheckOp used to persist a corrected status from `oddk list`;
//     that write is gone, and startup reconciliation owns store correction now.
//  2. Its reads must tolerate a concurrent writer. SQLite is opened in WAL mode
//     with busy_timeout=5000 (see store.New), so readers do not block on the
//     writer and vice versa. Docker inspects and PostgreSQL connects are
//     naturally concurrent.
//  3. A torn view is acceptable. A read here may observe a deployment
//     mid-operation — an instance that is being recreated, a backup row that is
//     about to exist. That is strictly better than the previous behaviour of
//     answering nothing at all, and it is what an operator watching a long
//     operation actually wants.
//
// The OpTypeWrite guard is a programming-error check, not input validation: a
// write routed here would silently lose the serialization the whole design rests
// on, so it fails loudly instead.
func (e *Executor) ExecuteRead(ctx context.Context, op Operation) error {
	if op.Type() != OpTypeRead {
		return fmt.Errorf("operation %s is not read-only and must go through Execute", op.Name())
	}
	return op.Execute(ctx)
}

// Close stops the executor accepting NEW operations. Whatever is in flight is
// left alone to finish — use Drain to wait for it.
//
// Reads are deliberately NOT refused after Close: they take no lock, change
// nothing, and staying answerable during a shutdown that is waiting on a long
// operation is the point of having a read path at all.
func (e *Executor) Close() {
	e.closing.Store(true)
}

// Drain blocks until no operation is running, or ctx is done.
//
// This is the graceful-shutdown half of "operations are uninterruptible":
// killing the process mid-operation is exactly what the design forbids, so a
// stop must wait. Scheduled operations (cron backups, snapshots) run in their
// own goroutine rather than an HTTP handler, so http.Server.Shutdown does not
// see them — this does.
func (e *Executor) Drain(ctx context.Context) error {
	idle := make(chan struct{})
	go func() {
		// Acquiring the lock IS the wait: it can only be taken once the
		// in-flight operation has released it.
		e.mu.Lock()
		e.mu.Unlock() //nolint:staticcheck // empty critical section is the point
		close(idle)
	}()

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ExecuteAsync runs an operation asynchronously but still sequentially
func (e *Executor) ExecuteAsync(ctx context.Context, op Operation) <-chan error {
	errCh := make(chan error, 1)

	go func() {
		errCh <- e.Execute(ctx, op)
		close(errCh)
	}()

	return errCh
}

// ListDatabasesOp executes the list databases operation
func (e *Executor) ListDatabasesOp(ctx context.Context, deps *Dependencies, params ListDatabasesParams) (*ListDatabasesResult, error) {
	var result *ListDatabasesResult
	op := &genericOp{
		name: "ListDatabases",
		typ:  OpTypeRead,
		fn: func(ctx context.Context) error {
			var err error
			result, err = ListDatabases(ctx, deps, params)
			return err
		},
	}

	// Read path: listing databases only queries PostgreSQL, so it must not be
	// blocked behind a long-running operation.
	err := e.ExecuteRead(ctx, op)
	return result, err
}

// CreateDatabaseOp executes the create database operation
func (e *Executor) CreateDatabaseOp(ctx context.Context, deps *Dependencies, params CreateDatabaseParams) (*CreateDatabaseResult, error) {
	var result *CreateDatabaseResult
	op := &genericOp{
		name: "CreateDatabase",
		typ:  OpTypeWrite,
		fn: func(ctx context.Context) error {
			var err error
			result, err = CreateDatabase(ctx, deps, params)
			return err
		},
	}

	err := e.Execute(ctx, op)
	return result, err
}

// AddDatabaseUserOp executes the add database user operation
func (e *Executor) AddDatabaseUserOp(ctx context.Context, deps *Dependencies, params AddDatabaseUserParams) (*AddDatabaseUserResult, error) {
	var result *AddDatabaseUserResult
	op := &genericOp{
		name: "AddDatabaseUser",
		typ:  OpTypeWrite,
		fn: func(ctx context.Context) error {
			var err error
			result, err = AddDatabaseUser(ctx, deps, params)
			return err
		},
	}

	err := e.Execute(ctx, op)
	return result, err
}

// DeleteDatabaseUserOp executes the delete database user operation
func (e *Executor) DeleteDatabaseUserOp(ctx context.Context, deps *Dependencies, params DeleteDatabaseUserParams) (*DeleteDatabaseUserResult, error) {
	var result *DeleteDatabaseUserResult
	op := &genericOp{
		name: "DeleteDatabaseUser",
		typ:  OpTypeWrite,
		fn: func(ctx context.Context) error {
			var err error
			result, err = DeleteDatabaseUser(ctx, deps, params)
			return err
		},
	}

	err := e.Execute(ctx, op)
	return result, err
}

// ResetDatabaseUserPasswordOp executes the reset database user password operation
func (e *Executor) ResetDatabaseUserPasswordOp(ctx context.Context, deps *Dependencies, params ResetDatabaseUserPasswordParams) (*ResetDatabaseUserPasswordResult, error) {
	var result *ResetDatabaseUserPasswordResult
	op := &genericOp{
		name: "ResetDatabaseUserPassword",
		typ:  OpTypeWrite,
		fn: func(ctx context.Context) error {
			var err error
			result, err = ResetDatabaseUserPassword(ctx, deps, params)
			return err
		},
	}

	err := e.Execute(ctx, op)
	return result, err
}

// genericOp is a generic operation wrapper
type genericOp struct {
	name string
	typ  OpType
	fn   func(context.Context) error
}

func (op *genericOp) Name() string {
	return op.name
}

func (op *genericOp) Type() OpType {
	return op.typ
}

func (op *genericOp) Execute(ctx context.Context) error {
	return op.fn(ctx)
}
