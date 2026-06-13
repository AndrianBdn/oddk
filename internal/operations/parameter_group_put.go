package operations

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hypersequent/oddk/internal/store/parameters"
)

type ParameterGroupPutResult struct {
	Message string `json:"message"`
}

func ParameterGroupPut(ctx context.Context, deps *Dependencies, params ParameterGroupPutParams) (*ParameterGroupPutResult, error) {
	paramStore := deps.Store.Parameters

	if params.Name == "" {
		return nil, fmt.Errorf("parameter group name is required")
	}

	var paramList []parameters.Parameter
	if err := json.Unmarshal(params.Parameters, &paramList); err != nil {
		return nil, fmt.Errorf("parse parameters JSON: %w", err)
	}

	// Basic validation - check parameter structure
	for i, param := range paramList {
		if param.Name == "" {
			return nil, fmt.Errorf("parameter %d: name is required", i)
		}
		if param.Type == "" {
			return nil, fmt.Errorf("parameter %d (%s): type is required", i, param.Name)
		}
		if param.ValueType == "" {
			return nil, fmt.Errorf("parameter %d (%s): value_type is required", i, param.Name)
		}
		if param.Value == "" {
			return nil, fmt.Errorf("parameter %d (%s): value is required", i, param.Name)
		}
	}

	// Try to resolve the parameter group with dummy values to validate expressions
	// Use dummy values for validation: 8 cores, 8GB RAM
	testCoreCount := 8
	testMemoryMB := 8 * 1024

	// Validate by trying to resolve the parameters directly (no need for temp DB storage)
	_, err := parameters.ResolveParameters(paramList, testCoreCount, testMemoryMB)
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
