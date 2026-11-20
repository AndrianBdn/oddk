package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store/parameters"
)

// parameterProbeUser runs the probe. The postgres server binary refuses to
// run as root, and the probe writes nothing, so it does not need the daemon's
// uid (which may be root). 65534 is "nobody" in the Debian and Alpine images.
const parameterProbeUser = "65534:65534"

// parameterProbePathGUCs are left out of the probe: they name files or
// directories that exist inside the real instance's volume but not in the
// throwaway probe, so the probe would refuse a group the real server accepts.
// The probe sets config_file itself.
var parameterProbePathGUCs = map[string]bool{
	"config_file":    true,
	"data_directory": true,
}

// checkTargetAcceptsParameterGroup checks, before anything is destroyed, that
// the target image's server accepts the instance's parameter group.
//
// It exists for major-upgrade, which removes the old volume BEFORE it starts
// the target cluster: a group the new major rejects (PostgreSQL 19 removed
// io_workers, which only PostgreSQL 18 had; an unknown -c setting is FATAL)
// would otherwise surface only after the data is gone, leaving the instance in
// error with the pre-upgrade backup as the only way back.
//
// Three checks, cheapest first:
//   - the group resolves for this instance's CPU/RAM;
//   - the resolved group fits the instance's RAM (the same estimate
//     CreateContainer applies, which otherwise runs only after the volume
//     removal);
//   - the target image's own server binary accepts each setting's name and
//     value. It runs `postgres -C server_version_num` with the group's exact
//     -c arguments (docker.PostgresConfigArgs, the list the instance container
//     gets); PostgreSQL parses and validates every -c before it answers -C.
//     Extension settings (pg_stat_statements.max) are accepted as
//     placeholders, as they are at real startup.
//
// What -C does NOT reach, because postgres answers it and exits first: checks
// that combine settings (wal_level=minimal with max_wal_senders > 0), loading
// shared_preload_libraries (a library the target image lacks), and allocating
// shared memory. Those still fail only at the new cluster's first start.
//
// Only PostgreSQL's own refusal (a FATAL/ERROR line from the probe) is
// operr.ErrInvalid (400). Anything else (Docker could not run the probe, it
// was killed, it exited without saying why) is an untagged error (500). Both
// stop the upgrade before the backup and the volume removal.
func checkTargetAcceptsParameterGroup(ctx context.Context, deps *Dependencies, instanceName, image string, targetMajor int, groupName string, params []parameters.Parameter, cpuCores, ramMB int) error {
	resolved, err := parameters.ResolveParameters(params, cpuCores, ramMB)
	if err != nil {
		return operr.Invalidf("parameter group %q does not resolve for this instance (%d CPU, %d MB RAM): %w; nothing was changed", groupName, cpuCores, ramMB, err)
	}
	if err := parameters.ValidateForContainer(resolved, ramMB); err != nil {
		return operr.Invalidf("parameter group %q: %w; nothing was changed", groupName, err)
	}

	err = runHelperContainer(ctx, deps, helperContainerSpec{
		ContainerName: fmt.Sprintf("oddk-paramcheck-%s-%d", instanceName, time.Now().UnixNano()),
		Image:         image,
		Entrypoint:    []string{"postgres"},
		Cmd:           parameterProbeCmd(resolved),
		// probeRefusal reads PostgreSQL's severity tags, which follow
		// lc_messages: a group setting it to a non-English locale (in an image
		// that has the locale; the official ones ship the translations)
		// translates "FATAL:". gettext consults LANGUAGE before the locale, and
		// there is no English catalog, so this keeps the original text while
		// lc_messages itself is still validated.
		Env:           []string{"LANGUAGE=en"},
		NoCredentials: true,
		User:          parameterProbeUser,
		NoNetwork:     true,
		Tool:          "postgres -C",
	})
	if err == nil {
		return nil
	}
	if exitErr, ok := errors.AsType[*helperExitError](err); ok {
		if refusal, ok := probeRefusal(exitErr); ok {
			return operr.Invalidf("PostgreSQL %d (image %s) refuses parameter group %q: %s. "+
				"Remove or change that setting in the group ('oddk parameters get --name %s', then 'oddk parameters put'), "+
				"or move the instance to another group first ('oddk instance apply %s --parameter-group <group>'), and retry; nothing was changed",
				targetMajor, image, groupName, refusal, groupName, instanceName)
		}
		// Output, not Logs: Logs substitutes the disk-full hint for empty
		// output, and the probe mounts nothing from the backup directory.
		output := exitErr.Output.String()
		switch {
		case output == "" && exitErr.Logs != noHelperOutputHint:
			output = exitErr.Logs // "<logs unavailable: ...>"
		case output == "":
			output = "no output"
		}
		return fmt.Errorf("check parameter group %q against %s: the probe exited %d without naming a setting (%s); nothing was changed",
			groupName, image, exitErr.Status, output)
	}
	return fmt.Errorf("check parameter group %q against %s: %w; nothing was changed", groupName, image, err)
}

// parameterProbeCmd is the probe's argument list for the server binary: a
// stand-in data directory (it must exist; nothing is read from it but an
// absent postgresql.auto.conf), an empty config file, the group's own -c
// arguments minus parameterProbePathGUCs, and a -C that makes postgres print
// one value and exit instead of starting.
func parameterProbeCmd(resolved []parameters.ResolvedParameter) []string {
	probed := make([]parameters.ResolvedParameter, 0, len(resolved))
	for _, p := range resolved {
		if parameterProbePathGUCs[strings.ToLower(p.Name)] {
			continue
		}
		probed = append(probed, p)
	}
	cmd := []string{"-D", "/tmp", "-c", "config_file=/dev/null"}
	cmd = append(cmd, docker.PostgresConfigArgs(probed)...)
	return append(cmd, "-C", "server_version_num")
}

// probeRefusal extracts PostgreSQL's own complaint from the probe's output:
// the FATAL/ERROR lines, minus the log-line prefix. ok is false when there is
// none, which means the probe did not fail on a setting (killed, out of
// memory, or a group that silenced its own messages with log_min_messages).
func probeRefusal(e *helperExitError) (string, bool) {
	var lines []string
	for line := range strings.SplitSeq(e.Output.stdout+"\n"+e.Output.stderr, "\n") {
		for _, severity := range []string{"FATAL:", "ERROR:"} {
			if _, msg, ok := strings.Cut(line, severity); ok {
				lines = append(lines, strings.TrimSpace(msg))
				break
			}
		}
	}
	return strings.Join(lines, "; "), len(lines) > 0
}
