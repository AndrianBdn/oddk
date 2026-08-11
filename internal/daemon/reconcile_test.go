package daemon

import (
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/store/instances"
)

// Startup reconciliation used to enumerate the four (storedStatus, dockerState)
// pairs it knew about and silently leave every other combination untouched.
// That is how "switching", "upgrading", "reconfiguring" and the "broken*"
// family came to survive every daemon restart forever — and a stuck status is
// not cosmetic, because the health checker skips any instance whose status is
// not "running", so the deployment reads green while an instance is unmonitored.
//
// These tests exist to make the matrix closed. `grep reconcileInstances
// --include=*_test.go` returning nothing is why the gap shipped.

// everyStoredStatus comes from the vocabulary itself, so a status added without
// being classified is caught here rather than quietly skipped by a hand-written
// list that nobody remembered to update.
var everyStoredStatus = instances.AllStatuses()

// everyContainerState is the closed set docker.Client.GetContainerStatus can
// return, plus "" for an instance with no container ID.
var everyContainerState = []string{"", "running", "stopped", "paused", "restarting", "not found"}

// TestDecideReconcile_MatrixIsClosed is the core guard: no combination may be
// left in a transient or unrecognised status. Either reconcile resolves it to a
// terminal status, or the status was already terminal/derived.
func TestDecideReconcile_MatrixIsClosed(t *testing.T) {
	for _, stored := range everyStoredStatus {
		for _, state := range everyContainerState {
			containerID := "container-id"
			if state == "" {
				containerID = ""
			}

			newStatus, message := decideReconcile(stored, containerID, state)

			final := stored
			if newStatus != "" {
				final = newStatus
			}

			// paused/restarting deliberately leave the status alone: the
			// container is mid-transition and reconcile has nothing better to
			// say. Every other combination must land somewhere terminal.
			if state == "paused" || state == "restarting" {
				if message == "" {
					t.Errorf("stored=%q state=%q: left unchanged with no explanation", stored, state)
				}
				continue
			}

			if !final.IsTerminal() {
				t.Errorf("stored=%q state=%q: left in non-terminal status %q (message: %q).\n"+
					"Every startup combination must resolve to running/stopped/error — an instance "+
					"left in a transient status is skipped by health checks forever.",
					stored, state, final, message)
			}
		}
	}
}

// A status change must always be explained. Silent status rewrites are how an
// operator loses track of what the daemon did to their deployment.
func TestDecideReconcile_EveryChangeIsLogged(t *testing.T) {
	for _, stored := range everyStoredStatus {
		for _, state := range everyContainerState {
			containerID := "container-id"
			if state == "" {
				containerID = ""
			}
			newStatus, message := decideReconcile(stored, containerID, state)
			if newStatus != "" && newStatus != stored && strings.TrimSpace(message) == "" {
				t.Errorf("stored=%q state=%q: changed to %q with no log message", stored, state, newStatus)
			}
		}
	}
}

// 'error' is never auto-cleared — by convention only an explicit operation
// promotes out of it. A container that happens to be up does not prove the
// instance is well: 'restoring' becomes 'error' precisely because its data may
// be incomplete while its server answers.
func TestDecideReconcile_ErrorIsNeverAutoCleared(t *testing.T) {
	for _, state := range everyContainerState {
		containerID := "container-id"
		if state == "" {
			containerID = ""
		}
		newStatus, _ := decideReconcile("error", containerID, state)
		if newStatus != "" && newStatus != "error" {
			t.Errorf("container state %q promoted 'error' to %q — only an explicit operation may do that", state, newStatus)
		}
	}
}

// The regression that motivated the fix: an interrupted operation must not
// leave its transient status behind.
func TestDecideReconcile_InterruptedStatusesResolveToError(t *testing.T) {
	for _, stored := range everyStoredStatus {
		remedy, interrupted := stored.IsInterrupted()
		if !interrupted {
			continue
		}
		for _, state := range everyContainerState {
			containerID := "container-id"
			if state == "" {
				containerID = ""
			}
			newStatus, message := decideReconcile(stored, containerID, state)
			if newStatus != "error" {
				t.Errorf("stored=%q state=%q: got %q, want 'error' — the operation that set it died mid-flight",
					stored, state, newStatus)
			}
			// The operator needs to know what to do next, not just that
			// something is wrong.
			if !strings.Contains(message, remedy) {
				t.Errorf("stored=%q: message %q does not carry the remedy %q", stored, message, remedy)
			}
		}
	}
}

// The "broken*" family was never written by an operation — ConsistencyCheckOp
// latched it onto healthy instances from an ordinary read command and never
// cleared it. Reconcile must un-latch those rows from the container's state,
// which is what an already-affected deployment needs on its next restart.
func TestDecideReconcile_BrokenFamilyIsUnlatched(t *testing.T) {
	for _, stored := range []instances.InstanceStatus{instances.StatusBroken, instances.StatusBrokenPort, instances.StatusBrokenAuth} {
		t.Run(string(stored)+"/running", func(t *testing.T) {
			newStatus, _ := decideReconcile(stored, "container-id", "running")
			if newStatus != "running" {
				t.Fatalf("got %q, want 'running' — a healthy container must clear a latched %q", newStatus, stored)
			}
		})
		t.Run(string(stored)+"/stopped", func(t *testing.T) {
			newStatus, _ := decideReconcile(stored, "container-id", "stopped")
			if newStatus != "stopped" {
				t.Fatalf("got %q, want 'stopped'", newStatus)
			}
		})
	}
}

// An unclassified status means someone added one without teaching reconcile
// about it. That must be loud, not silent — silence is the original bug.
func TestDecideReconcile_UnknownStatusFailsLoudly(t *testing.T) {
	newStatus, message := decideReconcile("teleporting", "container-id", "running")
	if newStatus != "error" {
		t.Fatalf("got %q, want 'error' for an unclassified status", newStatus)
	}
	if !strings.Contains(message, "UNRECOGNISED") || !strings.Contains(message, "status.go") {
		t.Errorf("message must name the problem and where to fix it, got %q", message)
	}
}

// Every classified status must appear in exactly one of the two maps, and the
// three terminal statuses must not be classified as interrupted.
func TestStatusClassificationIsConsistent(t *testing.T) {
	for _, status := range everyStoredStatus {
		_, interrupted := status.IsInterrupted()
		if interrupted && status.IsDerived() {
			t.Errorf("status %q is classified as BOTH interrupted and derived", status)
		}
		if !status.Valid() {
			t.Errorf("status %q is in AllStatuses but Valid() rejects it", status)
		}
		if status != instances.StatusError && !interrupted && !status.IsDerived() {
			t.Errorf("status %q is in AllStatuses but classified nowhere — "+
				"classify it in internal/store/instances/status.go", status)
		}
	}
	if _, interrupted := instances.StatusError.IsInterrupted(); interrupted || instances.StatusError.IsDerived() {
		t.Error("'error' must be neither interrupted nor derived — it is terminal and handled explicitly")
	}
}

func TestDecideReconcile_NoContainerID(t *testing.T) {
	newStatus, message := decideReconcile("running", "", "")
	if newStatus != "error" {
		t.Fatalf("got %q, want 'error' — a running instance with no container ID is broken", newStatus)
	}
	if !strings.Contains(message, "no container ID") {
		t.Errorf("message should say why, got %q", message)
	}

	// Already 'error' with no container: nothing to say, nothing to change.
	newStatus, message = decideReconcile("error", "", "")
	if newStatus != "" || message != "" {
		t.Errorf("an 'error' instance with no container needs no action, got (%q, %q)", newStatus, message)
	}
}

func TestDecideReconcile_AgreementIsSilent(t *testing.T) {
	for _, s := range []instances.InstanceStatus{instances.StatusRunning, instances.StatusStopped} {
		newStatus, message := decideReconcile(s, "container-id", string(s))
		if newStatus != "" || message != "" {
			t.Errorf("stored=%q state=%q: store and Docker agree, expected no action, got (%q, %q)",
				s, s, newStatus, message)
		}
	}
}
