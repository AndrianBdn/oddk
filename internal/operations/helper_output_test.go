package operations

import (
	"strings"
	"testing"
)

// A failed helper's output is the operator's only explanation for why a capture
// degraded, so "the helper said nothing" must be distinguishable from "the
// helper said something". It was not: getContainerLogs always returned a
// preformatted "stdout: %s\nstderr: %s", so a silent helper produced
//
//	pg_basebackup failed with status 1: stdout:
//	stderr:
//
// which reads like a formatting bug rather than information. Observed for real
// under disk exhaustion, where pg_basebackup fills the filesystem and Docker
// then cannot append its "No space left on device" line to a json log living on
// that same full filesystem.
func TestHelperOutput_EmptyIsDistinguishable(t *testing.T) {
	for _, tc := range []struct {
		name           string
		out            helperOutput
		wantNonEmpty   bool
		wantContaining string
	}{
		{name: "nothing at all", out: helperOutput{}},
		{name: "whitespace only", out: helperOutput{stdout: "\n", stderr: "  \n\t"}},
		{
			name:           "stderr carries the cause",
			out:            helperOutput{stderr: "pg_basebackup: error: No space left on device"},
			wantNonEmpty:   true,
			wantContaining: "No space left on device",
		},
		{
			name:           "stdout only",
			out:            helperOutput{stdout: "pg_dump: dumping contents"},
			wantNonEmpty:   true,
			wantContaining: "dumping contents",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.out.String()
			if tc.wantNonEmpty {
				if got == "" {
					t.Fatal("real helper output was rendered as empty, so the caller would replace it with the no-output hint and hide the actual cause")
				}
				if !strings.Contains(got, tc.wantContaining) {
					t.Errorf("rendered output %q lost %q", got, tc.wantContaining)
				}
				return
			}
			if got != "" {
				t.Errorf("a silent helper rendered as %q; it must be empty so the caller can substitute an explanation", got)
			}
		})
	}
}

// The substituted explanation has to name the check worth running first,
// because the failure it most often accompanies is the one that deleted its own
// error message.
func TestNoHelperOutputHint_PointsAtFreeSpace(t *testing.T) {
	for _, want := range []string{"no output", "full", "free space"} {
		if !strings.Contains(noHelperOutputHint, want) {
			t.Errorf("hint %q does not mention %q", noHelperOutputHint, want)
		}
	}
}
