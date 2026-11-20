package operations

import (
	"slices"
	"testing"

	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/store/parameters"
)

// The probe is only as good as its argument list: it must hand the target
// server the same -c list the instance container gets (so a setting the new
// major rejects is caught), minus the path settings that only resolve inside
// the real volume (so a group the real server accepts is not refused), and it
// must end in -C so postgres answers and exits instead of starting.
func TestParameterProbeCmd(t *testing.T) {
	resolved := []parameters.ResolvedParameter{
		{Name: "io_workers", Type: parameters.ParameterTypePostgresCLIArg, Value: "3"},
		{Name: "Data_Directory", Type: parameters.ParameterTypePostgresCLIArg, Value: "/var/lib/postgresql/data/pg"},
		{Name: "config_file", Type: parameters.ParameterTypePostgresCLIArg, Value: "/etc/pg.conf"},
		{Name: "pg_stat_statements.max", Type: parameters.ParameterTypePostgresCLIArg, Value: "1000"},
		{Name: "ignored_type", Type: "env", Value: "x"},
	}
	got := parameterProbeCmd(resolved)
	want := []string{
		"-D", "/tmp", "-c", "config_file=/dev/null",
		"-c", "io_workers=3",
		"-c", "pg_stat_statements.max=1000",
		"-C", "server_version_num",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parameterProbeCmd:\n got %q\nwant %q", got, want)
	}

	// Without path settings, the group part is exactly the container's list.
	plain := []parameters.ResolvedParameter{
		{Name: "max_connections", Type: parameters.ParameterTypePostgresCLIArg, Value: "200"},
		{Name: "shared_buffers", Type: parameters.ParameterTypePostgresCLIArg, Value: "1GB"},
	}
	cmd := parameterProbeCmd(plain)
	if group := cmd[4 : len(cmd)-2]; !slices.Equal(group, docker.PostgresConfigArgs(plain)) {
		t.Fatalf("probe group args %q differ from container args %q", group, docker.PostgresConfigArgs(plain))
	}
}

// The operator sees PostgreSQL's own complaint, not the log-line prefix or the
// LOG noise around it. Output that names no setting is not a refusal: it must
// not become a 400 telling the operator to edit a group that is fine.
func TestProbeRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    *helperExitError
		want   string
		wantOK bool
	}{
		{
			name: "unknown setting",
			err: &helperExitError{Status: 1, Output: helperOutput{
				stderr: "2026-11-20 08:01:09.679 GMT [1] FATAL:  unrecognized configuration parameter \"io_workers\"\n",
			}},
			want:   `unrecognized configuration parameter "io_workers"`,
			wantOK: true,
		},
		{
			name: "bad value among log noise",
			err: &helperExitError{Status: 1, Output: helperOutput{
				stderr: "2026-11-20 08:01:08.212 GMT [1] LOG:  skipping missing configuration file \"/tmp/postgresql.auto.conf\"\n" +
					"2026-11-20 08:01:08.979 GMT [1] FATAL:  invalid value for parameter \"lc_messages\": \"xx_YY\"\n",
			}},
			want:   `invalid value for parameter "lc_messages": "xx_YY"`,
			wantOK: true,
		},
		{
			name: "killed (137) with no output",
			err:  &helperExitError{Status: 137, Logs: noHelperOutputHint},
		},
		{
			name: "no severity line",
			err: &helperExitError{Status: 2, Output: helperOutput{
				stderr: "postgres: could not access directory \"/tmp\": No such file or directory",
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := probeRefusal(tc.err)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("probeRefusal = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
