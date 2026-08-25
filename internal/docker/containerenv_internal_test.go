package docker

import (
	"slices"
	"strings"
	"testing"
)

// The password belongs in Config.Env only when the container's first start will
// run initdb. Docker keeps Config.Env for the container's whole lifetime, so a
// credential the entrypoint discards is exposure with no function — see the
// comment on containerEnv.
func TestContainerEnvOmitsPasswordWhenInitdbWillNotRun(t *testing.T) {
	env := containerEnv("")

	for _, e := range env {
		if strings.HasPrefix(e, "POSTGRES_PASSWORD") {
			t.Fatalf("POSTGRES_PASSWORD must not be set when no password is supplied, got %q", e)
		}
	}
	if !slices.Contains(env, "POSTGRES_USER=postgres") {
		t.Fatalf("POSTGRES_USER must still be set, got %v", env)
	}
}

func TestContainerEnvCarriesPasswordForInitdb(t *testing.T) {
	env := containerEnv("s3cr3t")

	if !slices.Contains(env, "POSTGRES_PASSWORD=s3cr3t") {
		t.Fatalf("POSTGRES_PASSWORD must be set for an initdb start, got %v", env)
	}
	if !slices.Contains(env, "POSTGRES_USER=postgres") {
		t.Fatalf("POSTGRES_USER must be set, got %v", env)
	}
}
