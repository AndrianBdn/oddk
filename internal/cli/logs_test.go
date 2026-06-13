package cli_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypersequent/oddk/internal/cli"
)

func TestLogsAction_NoInstanceName(t *testing.T) {
	var buf bytes.Buffer
	err := cli.Run([]string{"oddk", "instance", "logs"}, nil, &buf)
	if err == nil {
		t.Fatal("expected error when no instance name provided")
	}
	if !strings.Contains(err.Error(), "instance name required") {
		t.Errorf("expected 'instance name required' error, got: %v", err)
	}
}

func TestLogsAction_InstanceNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": "instance not found"}`))
	}))
	defer server.Close()

	env := []string{fmt.Sprintf("ODDK_CLI_CONFIG=%s", writeTestConfig(t, server.URL))}

	var buf bytes.Buffer
	err := cli.Run([]string{"oddk", "instance", "logs", "nonexistent"}, env, &buf)
	if err == nil {
		t.Fatal("expected error for nonexistent instance")
	}
	if !strings.Contains(err.Error(), "instance not found") {
		t.Errorf("expected 'instance not found' error, got: %v", err)
	}
}

func TestLogsAction_ReturnsLogs(t *testing.T) {
	const fakeLogs = "LOG:  database system is ready to accept connections\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"logs": %q}`, fakeLogs)
	}))
	defer server.Close()

	env := []string{fmt.Sprintf("ODDK_CLI_CONFIG=%s", writeTestConfig(t, server.URL))}

	var buf bytes.Buffer
	if err := cli.Run([]string{"oddk", "instance", "logs", "myapp"}, env, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "database system is ready") {
		t.Errorf("expected log output in response, got: %q", buf.String())
	}
}

func TestLogsAction_TailQueryParam(t *testing.T) {
	var receivedURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"logs": ""}`))
	}))
	defer server.Close()

	env := []string{fmt.Sprintf("ODDK_CLI_CONFIG=%s", writeTestConfig(t, server.URL))}

	var buf bytes.Buffer
	_ = cli.Run([]string{"oddk", "instance", "logs", "myapp", "--tail", "50"}, env, &buf)

	if !strings.Contains(receivedURL, "tail=50") {
		t.Errorf("expected tail=50 in request URL, got: %s", receivedURL)
	}
}

// writeTestConfig writes a minimal CLI config pointing at the given daemon URL
func writeTestConfig(t *testing.T, daemonURL string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), ".oddk-cli.json")
	content := fmt.Sprintf(`{"daemonUrl": %q, "authToken": "test:dGVzdA=="}`, daemonURL)
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return configPath
}
