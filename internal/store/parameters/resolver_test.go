package parameters_test

import (
	"testing"

	"github.com/hypersequent/oddk/internal/store/parameters"
)

func TestResolveParameters(t *testing.T) {
	testCases := []struct {
		name       string
		parameters []parameters.Parameter
		coreCount  int
		memoryMB   int
		expected   map[string]string
		expectErr  bool
	}{
		{
			name: "basic parameter resolution",
			parameters: []parameters.Parameter{
				{
					Name:      "test_param",
					Type:      "postgres_cli_arg",
					ValueType: "numeric",
					Value:     "42",
				},
				{
					Name:      "memory_param",
					Type:      "postgres_cli_arg",
					ValueType: "numeric_mem",
					Value:     "{expr}DBContainerMemoryMB / 2{/expr} MB",
				},
			},
			coreCount: 4,
			memoryMB:  1024,
			expected: map[string]string{
				"test_param":   "42",
				"memory_param": "512 MB",
			},
			expectErr: false,
		},
		{
			name: "max_connections with dependent parameter",
			parameters: []parameters.Parameter{
				{
					Name:      "max_connections",
					Type:      "postgres_cli_arg",
					ValueType: "numeric",
					Value:     "{expr}round(DBContainerMemoryMB / 16){/expr}",
				},
				{
					Name:      "work_mem",
					Type:      "postgres_cli_arg",
					ValueType: "numeric_mem",
					Value:     "{expr}min(max(round((DBContainerMemoryMB * 0.25) / MaxConnections), 4), 64){/expr} MB",
				},
			},
			coreCount: 4,
			memoryMB:  2048,
			expected: map[string]string{
				"max_connections": "128",
				"work_mem":        "4 MB", // (2048 * 0.25) / 128 = 4
			},
			expectErr: false,
		},
		{
			name: "built-in math functions",
			parameters: []parameters.Parameter{
				{
					Name:      "test_min_max",
					Type:      "postgres_cli_arg",
					ValueType: "numeric",
					Value:     "{expr}max(min(DBContainerCoreCount * 4, 32), 8){/expr}",
				},
			},
			coreCount: 2,
			memoryMB:  1024,
			expected: map[string]string{
				"test_min_max": "8", // max(min(2*4, 32), 8) = max(8, 8) = 8
			},
			expectErr: false,
		},
		{
			name: "invalid expression",
			parameters: []parameters.Parameter{
				{
					Name:      "bad_param",
					Type:      "postgres_cli_arg",
					ValueType: "numeric",
					Value:     "{expr}invalid_expression_syntax{/expr}",
				},
			},
			coreCount: 4,
			memoryMB:  1024,
			expectErr: true,
		},
		{
			name: "invalid value type validation",
			parameters: []parameters.Parameter{
				{
					Name:      "bad_numeric_mem",
					Type:      "postgres_cli_arg",
					ValueType: "numeric_mem",
					Value:     "42", // Missing MB/GB suffix
				},
			},
			coreCount: 4,
			memoryMB:  1024,
			expectErr: true,
		},
		{
			name: "bool value types",
			parameters: []parameters.Parameter{
				{
					Name:      "bool_param_on",
					Type:      "postgres_cli_arg",
					ValueType: "bool",
					Value:     "on",
				},
				{
					Name:      "bool_param_true",
					Type:      "postgres_cli_arg",
					ValueType: "bool",
					Value:     "true",
				},
			},
			coreCount: 4,
			memoryMB:  1024,
			expected: map[string]string{
				"bool_param_on":   "on",
				"bool_param_true": "true",
			},
			expectErr: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := parameters.ResolveParameters(tc.parameters, tc.coreCount, tc.memoryMB)

			if tc.expectErr {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if len(resolved) != len(tc.expected) {
				t.Errorf("expected %d resolved parameters, got %d", len(tc.expected), len(resolved))
				return
			}

			resolvedMap := make(map[string]string)
			for _, r := range resolved {
				resolvedMap[r.Name] = r.Value
			}

			for name, expectedValue := range tc.expected {
				if actualValue, ok := resolvedMap[name]; !ok {
					t.Errorf("missing parameter %s", name)
				} else if actualValue != expectedValue {
					t.Errorf("parameter %s: expected %s, got %s", name, expectedValue, actualValue)
				}
			}
		})
	}
}

func TestValidateParameterValue(t *testing.T) {
	testCases := []struct {
		value     string
		valueType string
		expectErr bool
	}{
		{"42", "numeric", false},
		{"not_a_number", "numeric", true},
		{"256 MB", "numeric_mem", false},
		{"2 GB", "numeric_mem", false},
		{"256", "numeric_mem", true},    // Missing suffix
		{"256 KB", "numeric_mem", true}, // Wrong suffix
		{"any string", "string", false},
		{"on", "bool", false},
		{"off", "bool", false},
		{"true", "bool", false},
		{"false", "bool", false},
		{"yes", "bool", false},
		{"no", "bool", false},
		{"1", "bool", false},
		{"0", "bool", false},
		{"maybe", "bool", true},
		{"value", "invalid_type", true},
	}

	for _, tc := range testCases {
		t.Run(tc.value+"_"+tc.valueType, func(t *testing.T) {
			err := parameters.ValidateParameterValue(tc.value, tc.valueType)
			if tc.expectErr && err == nil {
				t.Errorf("expected error for value '%s' with type '%s'", tc.value, tc.valueType)
			} else if !tc.expectErr && err != nil {
				t.Errorf("unexpected error for value '%s' with type '%s': %v", tc.value, tc.valueType, err)
			}
		})
	}
}
