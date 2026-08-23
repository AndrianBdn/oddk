package operations

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseParameterListJSON_BareArray(t *testing.T) {
	raw := json.RawMessage(`[{"name":"max_connections","type":"postgres_cli_arg","valueType":"numeric","value":"75"}]`)
	list, err := parseParameterListJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "max_connections" {
		t.Fatalf("got %+v", list)
	}
}

func TestParseParameterListJSON_GetWrapper(t *testing.T) {
	raw := json.RawMessage(`{"groupName":"g","parameters":[{"name":"max_connections","type":"postgres_cli_arg","valueType":"numeric","value":"75"}]}`)
	list, err := parseParameterListJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "max_connections" {
		t.Fatalf("got %+v", list)
	}
}

func TestParseParameterListJSON_RejectsGarbage(t *testing.T) {
	_, err := parseParameterListJSON(json.RawMessage(`{"nope":true}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "expected a parameter array") {
		t.Fatalf("got %v", err)
	}
}
