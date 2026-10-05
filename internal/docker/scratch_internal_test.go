package docker

import (
	"slices"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/andrianbdn/oddk/internal/store/parameters"
)

// A scratch copy of a production cluster brings the source's subscriptions,
// foreign servers and scheduled jobs with it. Everything that keeps it from
// acting on real systems is a property of the container spec, so the spec is
// what is asserted.
func TestScratchContainerIsIsolated(t *testing.T) {
	spec := buildScratchContainer("app", "oddk-scratch-app-1", "17", "postgres:17", 2, 2048, []parameters.ResolvedParameter{
		{Name: "max_locks_per_transaction", Value: "512", Type: "postgres_cli_arg"},
		{Name: "max_logical_replication_workers", Value: "8", Type: "postgres_cli_arg"},
	})

	if spec.hostConfig.NetworkMode != container.NetworkMode("none") {
		t.Errorf("NetworkMode = %q, want none: a copy with network access can run the source's subscriptions and FDW queries", spec.hostConfig.NetworkMode)
	}
	if spec.networking != nil {
		t.Error("scratch container must not be attached to any network")
	}
	if len(spec.hostConfig.PortBindings) != 0 {
		t.Errorf("scratch container publishes ports: %v", spec.hostConfig.PortBindings)
	}

	// The instance's own parameters are kept (pg_dump needs the same lock
	// table), and the overrides come AFTER them, so they win.
	cmd := spec.config.Cmd
	locks := slices.Index(cmd, "max_locks_per_transaction=512")
	userWorkers := slices.Index(cmd, "max_logical_replication_workers=8")
	override := slices.Index(cmd, "max_logical_replication_workers=0")
	if locks < 0 {
		t.Errorf("the instance's parameters were dropped: %v", cmd)
	}
	if override < 0 || override < userWorkers {
		t.Errorf("max_logical_replication_workers=0 must come after the group's own value: %v", cmd)
	}
	for _, want := range []string{"archive_mode=off", "max_worker_processes=0", "default_transaction_read_only=on", "cron.launch_active_jobs=off"} {
		if !slices.Contains(cmd, want) {
			t.Errorf("%s missing: %v", want, cmd)
		}
	}

	if len(spec.hostConfig.Mounts) != 1 {
		t.Fatalf("mounts = %+v, want one anonymous volume", spec.hostConfig.Mounts)
	}
	m := spec.hostConfig.Mounts[0]
	// Its own labelled scratch volume (see scratchVolumeLabel), never the
	// instance's named volume.
	if m.Type != mount.TypeVolume || m.Source != "oddk-scratch-app-1" || m.Source == VolumeName("app") {
		t.Errorf("mount = %+v, want the scratch volume oddk-scratch-app-1", m)
	}
	if m.Target != "/var/lib/postgresql/data" {
		t.Errorf("mount target = %q", m.Target)
	}
	if spec.hostConfig.RestartPolicy.Name != container.RestartPolicyDisabled {
		t.Errorf("restart policy = %q; a failed recovery must fail the restore, not loop", spec.hostConfig.RestartPolicy.Name)
	}
	if spec.config.Labels["oddk.helper"] != "true" {
		t.Error("scratch container must carry oddk.helper=true so the startup sweep removes a crashed one")
	}
	if _, ok := spec.config.Labels["io.hpsq.oddk.instancename"]; ok {
		t.Error("scratch container must not carry the instance label: it is not the instance")
	}
	if slices.ContainsFunc(spec.config.Env, func(e string) bool { return len(e) > 17 && e[:17] == "POSTGRES_PASSWORD" }) {
		t.Error("scratch container must not carry a password: its volume is filled before the first start")
	}
}

func TestScratchContainerPG18MountsTheParent(t *testing.T) {
	spec := buildScratchContainer("app", "oddk-scratch-app-1", "18", "postgres:18", 1, 1024, nil)
	if got := spec.hostConfig.Mounts[0].Target; got != "/var/lib/postgresql" {
		t.Errorf("PG18 mount target = %q, want /var/lib/postgresql", got)
	}
}
