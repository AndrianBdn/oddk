package cli_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/andrianbdn/oddk/internal/store/parameters"
)

func TestParametersPutFileFormats(t *testing.T) {
	const list = `[{"name":"max_connections","type":"postgres_cli_arg","valueType":"numeric","value":"75"}]`
	for _, tc := range []struct {
		name string
		data string
	}{
		{"array", list},
		{"get-wrapper", `{"groupName":"source","parameters":` + list + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDaemon{}
			f.handle = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != "PUT" || r.URL.Path != "/api/parameters/target" {
					return false
				}
				var payload struct {
					Parameters []parameters.Parameter `json:"parameters"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode array payload: %v", err)
				} else if len(payload.Parameters) != 1 || payload.Parameters[0].Name != "max_connections" || payload.Parameters[0].Value != "75" {
					t.Errorf("unexpected parameters: %+v", payload.Parameters)
				}
				_, _ = w.Write([]byte(`{"message":"saved"}`))
				return true
			}
			path := filepath.Join(t.TempDir(), "params.json")
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := runCLI(t, f.start(t), "parameters", "put", "target", "--file", path); err != nil {
				t.Fatalf("put: %v (%s)", err, out)
			}
			if calls := f.recorded(); len(calls) != 1 || calls[0] != "PUT /api/parameters/target" {
				t.Fatalf("unexpected requests: %v", calls)
			}
		})
	}
}
