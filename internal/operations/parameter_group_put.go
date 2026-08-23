package operations

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/andrianbdn/oddk/internal/store/parameters"
)

type ParameterGroupPutResult struct {
	Message string `json:"message"`
}

func ParameterGroupPut(ctx context.Context, deps *Dependencies, params ParameterGroupPutParams) (*ParameterGroupPutResult, error) {
	paramStore := deps.Store.Parameters

	if params.Name == "" {
		return nil, fmt.Errorf("parameter group name is required")
	}

	paramList, err := parseParameterListJSON(params.Parameters)
	if err != nil {
		return nil, err
	}

	for i, param := range paramList {
		if err := parameters.ValidateParameterIdentity(param, i); err != nil {
			return nil, err
		}
	}

	// Try to resolve the parameter group with dummy values to validate expressions
	// Use dummy values for validation: 8 cores, 8GB RAM
	testCoreCount := 8
	testMemoryMB := 8 * 1024

	// Validate by trying to resolve the parameters directly (no need for temp DB storage)
	_, err = parameters.ResolveParameters(paramList, testCoreCount, testMemoryMB)
	if err != nil {
		return nil, fmt.Errorf("validation failed - parameter resolution: %w", err)
	}

	// Now create the actual parameter group
	if err := paramStore.CreateGroup(params.Name, paramList); err != nil {
		return nil, fmt.Errorf("create parameter group: %w", err)
	}

	return &ParameterGroupPutResult{
		Message: fmt.Sprintf("Parameter group '%s' created successfully", params.Name),
	}, nil
}

// parseParameterListJSON accepts either a bare parameter array or the
// {groupName, parameters} wrapper that `parameters get --json` emits, so GET
// output can be fed to PUT without jq.
func parseParameterListJSON(raw json.RawMessage) ([]parameters.Parameter, error) {
	var paramList []parameters.Parameter
	if err := json.Unmarshal(raw, &paramList); err == nil {
		return paramList, nil
	}
	var wrapped struct {
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil || len(wrapped.Parameters) == 0 {
		return nil, fmt.Errorf("parse parameters JSON: expected a parameter array or {\"parameters\": [...]}")
	}
	if err := json.Unmarshal(wrapped.Parameters, &paramList); err != nil {
		return nil, fmt.Errorf("parse parameters JSON: %w", err)
	}
	return paramList, nil
}
