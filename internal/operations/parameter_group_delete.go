package operations

import (
	"context"
	"fmt"
)

type ParameterGroupDeleteResult struct {
	Message string `json:"message"`
}

func ParameterGroupDelete(ctx context.Context, deps *Dependencies, params ParameterGroupDeleteParams) (*ParameterGroupDeleteResult, error) {
	paramStore := deps.Store.Parameters

	if params.Name == "" {
		return nil, fmt.Errorf("parameter group name is required")
	}

	if err := paramStore.DeleteGroup(params.Name); err != nil {
		return nil, fmt.Errorf("delete parameter group: %w", err)
	}

	return &ParameterGroupDeleteResult{
		Message: fmt.Sprintf("Parameter group '%s' deleted successfully", params.Name),
	}, nil
}
