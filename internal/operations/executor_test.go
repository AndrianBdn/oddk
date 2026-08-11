package operations

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeOp struct {
	name string
	typ  OpType
	fn   func(ctx context.Context) error
}

func (o *fakeOp) Name() string { return o.name }
func (o *fakeOp) Type() OpType { return o.typ }
func (o *fakeOp) Execute(ctx context.Context) error {
	if o.fn == nil {
		return nil
	}
	return o.fn(ctx)
}

// The regression this exists for: during a whole-deployment snapshot — minutes
// to hours — every read handler blocked on the executor's mutex and then died at
// the 30s HTTP WriteTimeout with a bare EOF. The commands an operator reaches
// for to find out what is happening were the ones that stopped answering while
// something was happening.
func TestExecuteRead_AnswersDuringALongWrite(t *testing.T) {
	e := NewExecutor()

	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	writeDone := make(chan struct{})

	go func() {
		defer close(writeDone)
		_ = e.Execute(context.Background(), &fakeOp{
			name: "LongSnapshot", typ: OpTypeWrite,
			fn: func(context.Context) error {
				close(writeStarted)
				<-releaseWrite
				return nil
			},
		})
	}()

	<-writeStarted // the lock is now held

	readReturned := make(chan error, 1)
	go func() {
		readReturned <- e.ExecuteRead(context.Background(), &fakeOp{name: "ListRDBMS", typ: OpTypeRead})
	}()

	select {
	case err := <-readReturned:
		if err != nil {
			t.Fatalf("read failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(releaseWrite)
		<-writeDone
		t.Fatal("a read blocked behind an in-flight write — this is the bug: " +
			"`oddk list` must answer during a long snapshot, not hang until the WriteTimeout")
	}

	close(releaseWrite)
	<-writeDone
}

// Writes must still serialize. If this ever passes concurrently, the executor's
// entire reason for existing is gone.
func TestExecute_WritesStillSerialize(t *testing.T) {
	e := NewExecutor()

	var mu sync.Mutex
	concurrent, maxConcurrent := 0, 0

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_ = e.Execute(context.Background(), &fakeOp{
				name: "Write", typ: OpTypeWrite,
				fn: func(context.Context) error {
					mu.Lock()
					concurrent++
					if concurrent > maxConcurrent {
						maxConcurrent = concurrent
					}
					mu.Unlock()

					time.Sleep(2 * time.Millisecond)

					mu.Lock()
					concurrent--
					mu.Unlock()
					return nil
				},
			})
		})
	}
	wg.Wait()

	if maxConcurrent != 1 {
		t.Errorf("observed %d concurrent write operations, want 1 — the serialization "+
			"that prevents half-aborted pg_restores and orphan containers is broken", maxConcurrent)
	}
}

// Routing a write through the read path would silently drop it out of the
// serialization the whole design rests on, so it must fail loudly.
func TestExecuteRead_RefusesWrites(t *testing.T) {
	e := NewExecutor()

	ran := false
	err := e.ExecuteRead(context.Background(), &fakeOp{
		name: "DestroyRDBMS", typ: OpTypeWrite,
		fn: func(context.Context) error { ran = true; return nil },
	})
	if err == nil {
		t.Fatal("ExecuteRead accepted an OpTypeWrite operation")
	}
	if ran {
		t.Error("the write actually executed — it must be refused before running")
	}
}

// Shutdown refuses new writes but must keep answering reads: staying observable
// while the drain waits on a long operation is the point of a read path.
func TestClose_RefusesWritesButNotReads(t *testing.T) {
	e := NewExecutor()
	e.Close()

	if err := e.Execute(context.Background(), &fakeOp{name: "Backup", typ: OpTypeWrite}); err == nil {
		t.Error("a write started after Close")
	}
	if err := e.ExecuteRead(context.Background(), &fakeOp{name: "ListRDBMS", typ: OpTypeRead}); err != nil {
		t.Errorf("a read was refused after Close: %v — reads take no lock and change nothing", err)
	}
}

// Drain is the graceful-shutdown half of "operations are uninterruptible": it
// must wait for the in-flight operation rather than let the process exit.
func TestDrain_WaitsForInFlightOperation(t *testing.T) {
	e := NewExecutor()

	started := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		_ = e.Execute(context.Background(), &fakeOp{
			name: "MajorUpgrade", typ: OpTypeWrite,
			fn: func(context.Context) error {
				close(started)
				time.Sleep(50 * time.Millisecond)
				close(finished)
				return nil
			},
		})
	}()

	<-started
	if err := e.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	select {
	case <-finished:
	default:
		t.Error("Drain returned while the operation was still running — " +
			"a major upgrade would be killed part-way")
	}
}

// If the caller's deadline expires first, Drain must report it rather than block
// forever: systemd's TimeoutStopSec is the real backstop and SIGKILL is worse.
func TestDrain_RespectsContextDeadline(t *testing.T) {
	e := NewExecutor()

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		_ = e.Execute(context.Background(), &fakeOp{
			name: "Endless", typ: OpTypeWrite,
			fn: func(context.Context) error {
				close(started)
				<-release
				return nil
			},
		})
	}()

	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := e.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Drain returned %v, want context.DeadlineExceeded", err)
	}

	close(release)
	<-done
}
