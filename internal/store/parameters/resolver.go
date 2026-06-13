package parameters

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/expr-lang/expr"
)

type ExpressionContext struct {
	DBContainerMemoryMB  int
	DBContainerCoreCount int
	MaxConnections       int
}

func (s *ParameterStore) ResolveParameterGroup(groupName string, coreCount, memoryMB int) ([]ResolvedParameter, error) {
	group, err := s.GetGroup(groupName)
	if err != nil {
		return nil, fmt.Errorf("get parameter group: %w", err)
	}

	return ResolveParameters(group.Parameters, coreCount, memoryMB)
}

// ResolveParameters resolves a list of parameters with the given system resources
// This function is decoupled from the store for easier testing
func ResolveParameters(parameters []Parameter, coreCount, memoryMB int) ([]ResolvedParameter, error) {
	// First pass: evaluate max_connections
	context := ExpressionContext{
		DBContainerMemoryMB:  memoryMB,
		DBContainerCoreCount: coreCount,
		MaxConnections:       0, // Will be set in first pass
	}

	var maxConnectionsValue int
	var resolvedParams []ResolvedParameter

	// Find and evaluate max_connections first
	for _, param := range parameters {
		if param.Name == "max_connections" {
			resolved, err := resolveParameter(param, context)
			if err != nil {
				return nil, fmt.Errorf("resolve max_connections: %w", err)
			}

			maxConnectionsValue, err = strconv.Atoi(resolved.Value)
			if err != nil {
				return nil, fmt.Errorf("parse max_connections value: %w", err)
			}

			context.MaxConnections = maxConnectionsValue
			resolvedParams = append(resolvedParams, *resolved)
			break
		}
	}

	// Second pass: evaluate all other parameters
	for _, param := range parameters {
		if param.Name == "max_connections" {
			continue // Already resolved
		}

		resolved, err := resolveParameter(param, context)
		if err != nil {
			return nil, fmt.Errorf("resolve parameter %s: %w", param.Name, err)
		}

		resolvedParams = append(resolvedParams, *resolved)
	}

	return resolvedParams, nil
}

func resolveParameter(param Parameter, context ExpressionContext) (*ResolvedParameter, error) {
	resolvedValue, err := evaluateExpression(param.Value, context)
	if err != nil {
		return nil, fmt.Errorf("evaluate expression: %w", err)
	}

	err = ValidateParameterValue(resolvedValue, param.ValueType)
	if err != nil {
		return nil, fmt.Errorf("validate parameter value: %w", err)
	}

	return &ResolvedParameter{
		Name:      param.Name,
		Type:      param.Type,
		ValueType: param.ValueType,
		Value:     resolvedValue,
	}, nil
}

func evaluateExpression(value string, context ExpressionContext) (string, error) {
	// Check if value contains expression tags
	exprRegex := regexp.MustCompile(`\{expr\}(.*?)\{/expr\}`)
	matches := exprRegex.FindStringSubmatch(value)

	if matches == nil {
		// No expression, return value as-is
		return value, nil
	}

	if len(matches) != 2 {
		return "", fmt.Errorf("invalid expression format")
	}

	expression := matches[1]

	env := map[string]any{
		"DBContainerMemoryMB":  context.DBContainerMemoryMB,
		"DBContainerCoreCount": context.DBContainerCoreCount,
		"MaxConnections":       context.MaxConnections,
	}

	program, err := expr.Compile(expression, expr.Env(env))
	if err != nil {
		return "", fmt.Errorf("compile expression '%s': %w", expression, err)
	}

	result, err := expr.Run(program, env)
	if err != nil {
		return "", fmt.Errorf("execute expression '%s': %w", expression, err)
	}

	// Convert result to string and replace in original value
	var resultStr string
	switch v := result.(type) {
	case int:
		resultStr = strconv.Itoa(v)
	case float64:
		resultStr = strconv.FormatFloat(v, 'f', -1, 64)
	default:
		resultStr = fmt.Sprintf("%v", result)
	}

	// Replace the expression with the result
	resolvedValue := exprRegex.ReplaceAllString(value, resultStr)

	return resolvedValue, nil
}

// ValidateParameterValue validates a parameter value against its type
func ValidateParameterValue(value, valueType string) error {
	switch valueType {
	case "numeric":
		_, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("value '%s' is not a valid numeric value: %w", value, err)
		}

	case "numeric_mem":
		// Should end with MB or GB
		value = strings.TrimSpace(value)
		if !strings.HasSuffix(value, " MB") && !strings.HasSuffix(value, " GB") {
			return fmt.Errorf("numeric_mem value '%s' must end with ' MB' or ' GB'", value)
		}

		var numPart string
		if n, ok := strings.CutSuffix(value, " MB"); ok {
			numPart = n
		} else {
			numPart, _ = strings.CutSuffix(value, " GB")
		}

		_, err := strconv.Atoi(numPart)
		if err != nil {
			return fmt.Errorf("numeric_mem value '%s' has invalid numeric part: %w", value, err)
		}

	case "string":
		// Any string is valid

	case "bool":
		lowerValue := strings.ToLower(strings.TrimSpace(value))
		validBools := []string{"on", "off", "true", "false", "yes", "no", "1", "0"}
		if slices.Contains(validBools, lowerValue) {
			return nil
		}
		return fmt.Errorf("bool value '%s' must be one of: %v", value, validBools)

	default:
		return fmt.Errorf("unknown value type: %s", valueType)
	}

	return nil
}
