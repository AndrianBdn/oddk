package operations

import (
	"testing"

	"github.com/andrianbdn/oddk/internal/store/instances"
)

// TestDeriveDisplayStatus pins the one rule that separates what a read command
// SHOWS from what the container reports: a stored "error" survives, everything
// else defers to Docker.
func TestDeriveDisplayStatus(t *testing.T) {
	containerStates := []instances.InstanceStatus{
		instances.StatusRunning,
		instances.StatusStopped,
		instances.StatusBroken,
		instances.StatusBrokenPort,
		instances.StatusBrokenAuth,
		"paused",     // Docker states this file passes through verbatim
		"restarting", // for display; never stored, so no CHECK applies
	}

	// An instance parked in "error" reads "error" whatever its container is
	// doing. A rolled-back apply leaves an exited container behind, and the row
	// is never auto-cleared, so rendering it "stopped" is a lie that persists.
	for _, cs := range containerStates {
		if got := deriveDisplayStatus(instances.StatusError, cs); got != instances.StatusError {
			t.Errorf("stored=error container=%s: got %s, want error", cs, got)
		}
	}

	// Every other stored status defers to the container — the behaviour this
	// file exists for, including the interrupted statuses, which reconciliation
	// resolves at startup rather than here.
	deferring := []instances.InstanceStatus{
		instances.StatusRunning,
		instances.StatusStopped,
		instances.StatusCreating,
		instances.StatusRestoring,
		instances.StatusSwitching,
		instances.StatusReconfiguring,
		instances.StatusUpgrading,
		instances.StatusBroken,
		instances.StatusBrokenPort,
		instances.StatusBrokenAuth,
	}
	for _, stored := range deferring {
		for _, cs := range containerStates {
			if got := deriveDisplayStatus(stored, cs); got != cs {
				t.Errorf("stored=%s container=%s: got %s, want %s", stored, cs, got, cs)
			}
		}
	}

	// A container that vanished still promotes TO error: Execute assigns that
	// one directly, so the helper must not stand in its way either.
	if got := deriveDisplayStatus(instances.StatusRunning, instances.StatusError); got != instances.StatusError {
		t.Errorf("stored=running container=error: got %s, want error", got)
	}
}
