package operations

import (
	"errors"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/store/instances"
)

// The regression this file exists for: snapshot capture used to dispatch on the
// instance's STORED status, so an instance whose status had drifted to anything
// other than "running"/"stopped" was reduced to configuration-only — silently,
// in every subsequent archive, while the snapshot itself reported success.
//
// Drift is not hypothetical. ConsistencyCheckOp latches "broken-port" onto a
// healthy instance from an ordinary `oddk list` during a blip and never clears
// it; a crashed switch/upgrade/reconfigure leaves its transient status behind
// and startup reconcile does not repair it. So the table below asserts the one
// property that matters: a container that is RUNNING gets its data captured,
// whatever the store believes about it.

func (a captureAction) String() string {
	switch a {
	case captureConfigOnly:
		return "configOnly"
	case captureBasebackup:
		return "basebackup"
	case captureCold:
		return "cold"
	case captureLogicalDump:
		return "logicalDump"
	}
	return "unknown"
}

// everyStoredStatus comes from the vocabulary itself, so a newly added status is
// covered here automatically rather than needing a hand-maintained copy.
var everyStoredStatus = instances.AllStatuses()

// TestDecideCapture_RunningContainerIsAlwaysCaptured is the core regression
// guard: whatever the stored status says, a running container's data must be
// captured. Before the fix, 9 of these 11 rows returned configOnly.
func TestDecideCapture_RunningContainerIsAlwaysCaptured(t *testing.T) {
	for _, stored := range everyStoredStatus {
		t.Run("physical/"+string(stored), func(t *testing.T) {
			action, reason := decideCapture(SnapshotFormatPhysical, stored, "container-id", "running", nil)
			if action != captureBasebackup {
				t.Fatalf("stored status %q with a RUNNING container: got %s (%s), want basebackup.\n"+
					"A running cluster must never be reduced to configuration-only because a TEXT column disagreed.",
					stored, action, reason)
			}
		})
		t.Run("logical/"+string(stored), func(t *testing.T) {
			action, reason := decideCapture(SnapshotFormatLogical, stored, "container-id", "running", nil)
			if action != captureLogicalDump {
				t.Fatalf("stored status %q with a RUNNING container: got %s (%s), want logicalDump",
					stored, action, reason)
			}
		})
	}
}

// TestDecideCapture_StoppedContainer: physical cold-copies it (a stopped data
// directory is a valid physical backup); logical cannot dump a dead server.
func TestDecideCapture_StoppedContainer(t *testing.T) {
	for _, stored := range everyStoredStatus {
		t.Run("physical/"+string(stored), func(t *testing.T) {
			action, reason := decideCapture(SnapshotFormatPhysical, stored, "container-id", "stopped", nil)
			if action != captureCold {
				t.Fatalf("stored status %q with a STOPPED container: got %s (%s), want cold", stored, action, reason)
			}
		})
		t.Run("logical/"+string(stored), func(t *testing.T) {
			action, reason := decideCapture(SnapshotFormatLogical, stored, "container-id", "stopped", nil)
			if action != captureConfigOnly {
				t.Fatalf("stored status %q with a STOPPED container: got %s, want configOnly (a dump needs a live server)", stored, action)
			}
			if !strings.Contains(reason, "live server") {
				t.Errorf("reason should explain why logical cannot capture a stopped container, got %q", reason)
			}
		})
	}
}

func TestDecideCapture_Uncapturable(t *testing.T) {
	tests := []struct {
		name        string
		format      string
		stored      instances.InstanceStatus
		containerID string
		actual      string
		stateErr    error
		wantReason  string
	}{
		{
			name:        "no container at all",
			format:      SnapshotFormatPhysical,
			stored:      instances.StatusError,
			containerID: "",
			wantReason:  "no container",
		},
		{
			name:        "container state unreadable",
			format:      SnapshotFormatPhysical,
			stored:      instances.StatusRunning,
			containerID: "container-id",
			stateErr:    errors.New("docker daemon unreachable"),
			wantReason:  "could not be determined",
		},
		{
			name:        "container vanished",
			format:      SnapshotFormatPhysical,
			stored:      instances.StatusRunning,
			containerID: "container-id",
			actual:      "not found",
			wantReason:  "no longer exists",
		},
		{
			name:        "paused: neither serving nor cleanly stopped",
			format:      SnapshotFormatPhysical,
			stored:      instances.StatusRunning,
			containerID: "container-id",
			actual:      "paused",
			wantReason:  "neither serving nor cleanly stopped",
		},
		{
			name:        "restarting: neither serving nor cleanly stopped",
			format:      SnapshotFormatPhysical,
			stored:      instances.StatusRunning,
			containerID: "container-id",
			actual:      "restarting",
			wantReason:  "neither serving nor cleanly stopped",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, reason := decideCapture(tt.format, tt.stored, tt.containerID, tt.actual, tt.stateErr)
			if action != captureConfigOnly {
				t.Fatalf("got %s, want configOnly", action)
			}
			if !strings.Contains(reason, tt.wantReason) {
				t.Errorf("reason %q does not mention %q — the manifest skipReason is the operator's only\n"+
					"explanation for why this instance has no data in the archive, so it must be specific",
					reason, tt.wantReason)
			}
		})
	}
}

// A stateErr must never be silently treated as "container is fine". It also must
// not abort the run: one unreadable container degrades one entry.
func TestDecideCapture_StateErrorBeatsActualState(t *testing.T) {
	action, reason := decideCapture(SnapshotFormatPhysical, "running", "container-id", "running", errors.New("boom"))
	if action != captureConfigOnly {
		t.Fatalf("a state-read error with a stale 'running' reading must not be captured as if healthy; got %s", action)
	}
	if !strings.Contains(reason, "boom") {
		t.Errorf("reason must carry the underlying error, got %q", reason)
	}
}

// configOnly reasons are written into the manifest and shown to whoever is
// deciding whether to trust an archive, so none may be empty.
func TestDecideCapture_ConfigOnlyAlwaysHasAReason(t *testing.T) {
	for _, format := range []string{SnapshotFormatPhysical, SnapshotFormatLogical} {
		for _, stored := range everyStoredStatus {
			for _, actual := range []string{"", "running", "stopped", "paused", "restarting", "not found"} {
				for _, id := range []string{"", "container-id"} {
					action, reason := decideCapture(format, stored, id, actual, nil)
					if action == captureConfigOnly && strings.TrimSpace(reason) == "" {
						t.Fatalf("configOnly with empty reason: format=%s stored=%s id=%q actual=%q", format, stored, id, actual)
					}
					if action != captureConfigOnly && reason != "" {
						t.Fatalf("non-configOnly action %s carried a reason %q: format=%s stored=%s actual=%q",
							action, reason, format, stored, actual)
					}
				}
			}
		}
	}
}
