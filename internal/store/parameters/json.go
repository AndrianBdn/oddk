package parameters

import (
	"encoding/json"
	"fmt"
)

// ParseListJSON accepts a bare array or the object emitted by parameters get --json.
func ParseListJSON(raw json.RawMessage) ([]Parameter, error) {
	var list []Parameter
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var wrapped struct {
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil || len(wrapped.Parameters) == 0 {
		return nil, fmt.Errorf("parse parameters JSON: expected a parameter array or {\"parameters\": [...]}")
	}
	if err := json.Unmarshal(wrapped.Parameters, &list); err != nil {
		return nil, fmt.Errorf("parse parameters JSON: %w", err)
	}
	return list, nil
}
