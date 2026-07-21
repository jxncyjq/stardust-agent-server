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
	// Group places this tool in the capability catalog, by what it is for
	// ("files", "tasks", "messages"), not by where it came from. It is what a
	// model scanning the catalog reads first, so it is required: an unplaced
	// tool cannot be listed.
	Group string
	// Sensitive 标记一个工具为有副作用：Manual 模式下对它的调用被挡在人工审批后
	// （M2b），Plan 模式把它排除出所提供的工具集。只读工具（read/search/list）非敏感。
	Sensitive bool `json:"sensitive,omitempty"`
}

// validateInputSchema checks args against a tool's declared input schema.
//
// A schema it cannot read is an error, never a pass. Every piece used to be
// taken with comma-ok and the failure discarded: malformed "properties" became
// nil so the loop never ran, a malformed property became nil whose ["type"] is
// nil which the switch waved through as "string", and a malformed "required"
// became an empty list. Any of those made this function return nil — "valid" —
// having checked nothing, and illegal or missing arguments went straight into
// handler.Execute. For write_file or send_message that is the first line of
// defence disappearing without a trace. Schemas are code-defined constants, so a
// malformed one is a programming error and gets reported as such.
func validateInputSchema(schema map[string]any, args map[string]string) error {
	if len(schema) == 0 {
		return nil
	}
	required, err := schemaStringList(schema["required"])
	if err != nil {
		return fmt.Errorf("validate tool input: schema %q malformed: %w", "required", err)
	}
	for _, name := range required {
		if _, ok := args[name]; !ok {
			return fmt.Errorf("validate tool input: required argument %q missing", name)
		}
	}
	properties := map[string]any{}
	if raw, present := schema["properties"]; present {
		typed, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("validate tool input: schema %q malformed: want object, got %T", "properties", raw)
		}
		properties = typed
	}
	for name, rawProperty := range properties {
		value, ok := args[name]
		if !ok {
			continue
		}
		property, ok := rawProperty.(map[string]any)
		if !ok {
			return fmt.Errorf("validate tool input: schema for argument %q malformed: want object, got %T", name, rawProperty)
		}
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

// schemaStringList reads a schema field that must be a list of strings.
//
// A nil value means the field was absent, which is legitimately optional and
// yields no names. Anything else that is not a list of strings is malformed:
// silently returning nil there used to turn the required-argument check into a
// no-op, so a call missing every required argument validated cleanly.
func schemaStringList(value any) ([]string, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case []string:
		return typed, nil
	case []any:
		values := make([]string, 0, len(typed))
		for i, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("item %d: want string, got %T", i, item)
			}
			values = append(values, text)
		}
		return values, nil
	default:
		return nil, fmt.Errorf("want list of strings, got %T", value)
	}
}
