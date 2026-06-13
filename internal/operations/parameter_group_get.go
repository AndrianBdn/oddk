package operations

import (
	"context"
	"fmt"

	"github.com/hypersequent/oddk/internal/store/parameters"
)

type ParameterGroupGetResult struct {
	GroupName  string                         `json:"groupName"`
	Parameters []parameters.ResolvedParameter `json:"parameters"`
}

func ParameterGroupGet(ctx context.Context, deps *Dependencies, params ParameterGroupGetParams) (*ParameterGroupGetResult, error) {
	paramStore := deps.Store.Parameters

	group, err := paramStore.GetGroup(params.Name)
	if err != nil {
		return nil, fmt.Errorf("get parameter group: %w", err)
	}

	// Convert parameters to resolved format (without evaluation)
	var resolvedParams []parameters.ResolvedParameter
	for _, param := range group.Parameters {
		resolvedParams = append(resolvedParams, parameters.ResolvedParameter{
			Name:      param.Name,
			Type:      param.Type,
			ValueType: param.ValueType,
			Value:     param.Value,
		})
	}

	return &ParameterGroupGetResult{
		GroupName:  group.Name,
		Parameters: resolvedParams,
	}, nil
}
