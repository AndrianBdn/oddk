package parameters_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/andrianbdn/oddk/internal/store/parameters"
)

func TestParseParameterListJSON_BareArray(t *testing.T) {
	raw := json.RawMessage(`[{"name":"max_connections","type":"postgres_cli_arg","valueType":"numeric","value":"75"}]`)
	list, err := parameters.ParseListJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "max_connections" {
		t.Fatalf("got %+v", list)
	}
}

func TestParseParameterListJSON_GetWrapper(t *testing.T) {
	raw := json.RawMessage(`{"groupName":"g","parameters":[{"name":"max_connections","type":"postgres_cli_arg","valueType":"numeric","value":"75"}]}`)
	list, err := parameters.ParseListJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "max_connections" {
		t.Fatalf("got %+v", list)
	}
}

func TestParseParameterListJSON_RejectsGarbage(t *testing.T) {
	_, err := parameters.ParseListJSON(json.RawMessage(`{"nope":true}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "expected a parameter array") {
		t.Fatalf("got %v", err)
	}
}

func TestParseListJSONInvalidShapes(t *testing.T) {
	for _, raw := range []string{`{`, `42`, `"text"`, `{"parameters":{}}`, `{"parameters":[42]}`, `[{"value":42}]`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parameters.ParseListJSON(json.RawMessage(raw)); err == nil {
				t.Fatal("expected parse error")
			}
		})
	}
}

func TestParseListJSONEmpty(t *testing.T) {
	for _, raw := range []string{`[]`, `null`, `{"parameters":[]}`, `{"parameters":null}`} {
		t.Run(raw, func(t *testing.T) {
			list, err := parameters.ParseListJSON(json.RawMessage(raw))
			if err != nil || len(list) != 0 {
				t.Fatalf("got (%v, %v), want empty list", list, err)
			}
		})
	}
}
