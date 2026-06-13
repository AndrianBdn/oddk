package operations

import (
	"context"
	"fmt"

	"github.com/hypersequent/oddk/internal/store/parameters"
)

type ParameterGroupListParams struct{}

type ParameterGroupListResult struct {
	Groups []ParameterGroupInfo `json:"groups"`
}

type ParameterGroupInfo struct {
	Name       string                         `json:"name"`
	Parameters []parameters.ResolvedParameter `json:"parameters"`
}

func ParameterGroupList(ctx context.Context, deps *Dependencies, params ParameterGroupListParams) (*ParameterGroupListResult, error) {
	paramStore := deps.Store.Parameters

	groupNames, err := paramStore.ListGroups()
	if err != nil {
		return nil, fmt.Errorf("list parameter groups: %w", err)
	}

	var groups []ParameterGroupInfo
	for _, groupName := range groupNames {
		group, err := paramStore.GetGroup(groupName)
		if err != nil {
			return nil, fmt.Errorf("get parameter group %s: %w", groupName, err)
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

		groups = append(groups, ParameterGroupInfo{
			Name:       groupName,
			Parameters: resolvedParams,
		})
	}

	return &ParameterGroupListResult{
		Groups: groups,
	}, nil
}
