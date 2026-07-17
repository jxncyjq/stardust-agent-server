package tool

import (
	"fmt"
	"strconv"
	"time"
)

type Descriptor struct {
	Name        string
	Description string
	InputSchema map[string]any
	RiskLevel   string
	Timeout     time.Duration
}

func validateInputSchema(schema map[string]any, args map[string]string) error {
	if len(schema) == 0 {
		return nil
	}
	for _, name := range schemaStringList(schema["required"]) {
		if _, ok := args[name]; !ok {
			return fmt.Errorf("validate tool input: required argument %q missing", name)
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for name, rawProperty := range properties {
		value, ok := args[name]
		if !ok {
			continue
		}
		property, _ := rawProperty.(map[string]any)
		switch property["type"] {
		case "number":
			if _, err := strconv.ParseFloat(value, 64); err != nil {
				return fmt.Errorf("validate tool input: argument %q must be number", name)
			}
		case "boolean":
			if _, err := strconv.ParseBool(value); err != nil {
				return fmt.Errorf("validate tool input: argument %q must be boolean", name)
			}
		case "string", nil, "":
		default:
			return fmt.Errorf("validate tool input: argument %q has unsupported schema type %q", name, property["type"])
		}
	}
	return nil
}

func schemaStringList(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				values = append(values, text)
			}
		}
		return values
	default:
		return nil
	}
}
