package cli_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/andrianbdn/oddk/internal/cli"
)

// fakeDaemon is the shared skeleton behind the CLI tests' fake daemons: it
// records every request as "METHOD /path", sets the JSON content type, and
// delegates routing to a per-test handler. A request the handler reports as
// unrecognised (or a nil handler) falls through to a JSON 404, the same shape
// each fake used before they were merged.
type fakeDaemon struct {
	mu    sync.Mutex
	calls []string
	// handle serves one request and reports whether it recognised the route.
	handle func(w http.ResponseWriter, r *http.Request) bool
}

func (f *fakeDaemon) record(method, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+" "+path)
}

func (f *fakeDaemon) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// start serves the fake daemon until test cleanup and returns the env that
// points the CLI at it.
func (f *fakeDaemon) start(t *testing.T) []string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if f.handle != nil && f.handle(w, r) {
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": "not found"}`))
	}))
	t.Cleanup(srv.Close)
	return []string{fmt.Sprintf("ODDK_CLI_CONFIG=%s", writeTestConfig(t, srv.URL))}
}

// runCLI runs one CLI invocation ("oddk" is implied) and captures its output.
func runCLI(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := cli.Run(append([]string{"oddk"}, args...), env, &buf)
	return buf.String(), err
}
