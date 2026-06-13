package operations

import (
	"context"
	"fmt"
	"log"
	"sync"
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
	running bool
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
// here is only useful if a caller intentionally wires in cancellation (e.g. a
// future graceful-shutdown context).
func (e *Executor) Execute(ctx context.Context, op Operation) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running {
		return fmt.Errorf("another operation is already running")
	}

	e.running = true
	defer func() {
		e.running = false
	}()

	log.Printf("Starting operation: %s", op.Name())

	err := op.Execute(ctx)
	if err != nil {
		log.Printf("Operation failed: %s - %v", op.Name(), err)
		return err
	}

	log.Printf("Operation completed: %s", op.Name())
	return nil
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

	err := e.Execute(ctx, op)
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
