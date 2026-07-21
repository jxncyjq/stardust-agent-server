package tool

import (
	"strings"
	"testing"
)

// Audit item V11: validateInputSchema reached for its pieces with comma-ok and
// discarded the failures.
//
// `properties, _ := schema["properties"].(map[string]any)` on a malformed schema
// yields nil, so the property loop never runs; `property, _ := ...` yields nil,
// whose ["type"] is nil, which the switch treats as "string" and waves through;
// and schemaStringList's default returns nil, so the required-argument check
// silently checks nothing. The result is that validateInputSchema returns nil —
// "valid" — having verified nothing at all, and illegal or missing arguments go
// straight into handler.Execute. For write_file or send_message that is the
// first line of defence disappearing without a trace.
//
// Schemas are code-defined constants, so a malformed one is a programming error
// and must be loud.

func TestValidateInputSchemaRejectsMalformedProperties(t *testing.T) {
	t.Parallel()

	// properties present but not an object: the whole property loop used to be
	// skipped, so this call reported success.
	err := validateInputSchema(map[string]any{"properties": []string{"path"}}, map[string]string{"path": "x"})
	if err == nil {
		t.Fatal("validateInputSchema(malformed properties) error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "properties") {
		t.Errorf("error = %q, want it to name the malformed part", err.Error())
	}
}

func TestValidateInputSchemaRejectsMalformedProperty(t *testing.T) {
	t.Parallel()

	// A single property that is not an object: property["type"] used to be nil,
	// which the switch accepted as "string".
	err := validateInputSchema(map[string]any{
		"properties": map[string]any{"count": "number"},
	}, map[string]string{"count": "not-a-number"})
	if err == nil {
		t.Fatal("validateInputSchema(malformed property) error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "count") {
		t.Errorf("error = %q, want it to name the offending property", err.Error())
	}
}

func TestValidateInputSchemaRejectsMalformedRequired(t *testing.T) {
	t.Parallel()

	// required present but not a list: the required check used to pass over it,
	// so a call missing every required argument validated fine.
	err := validateInputSchema(map[string]any{"required": "path"}, map[string]string{})
	if err == nil {
		t.Fatal("validateInputSchema(malformed required) error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %q, want it to name the malformed part", err.Error())
	}
}

func TestValidateInputSchemaRejectsNonStringInRequired(t *testing.T) {
	t.Parallel()

	// A []any whose items are not all strings: the non-string items used to be
	// dropped, so those arguments stopped being required.
	err := validateInputSchema(map[string]any{"required": []any{"path", 42}}, map[string]string{"path": "x"})
	if err == nil {
		t.Fatal("validateInputSchema(non-string in required) error = nil, want an error")
	}
}

// TestValidateInputSchemaStillAcceptsWellFormedSchemas guards the other
// direction: a validation that started rejecting legitimate schemas would break
// every tool call, which is far worse than the bug being fixed.
func TestValidateInputSchemaStillAcceptsWellFormedSchemas(t *testing.T) {
	t.Parallel()

	schema := map[string]any{
		"type":     "object",
		"required": []string{"path"},
		"properties": map[string]any{
			"path":      map[string]any{"type": "string"},
			"count":     map[string]any{"type": "number"},
			"recursive": map[string]any{"type": "boolean"},
			"untyped":   map[string]any{"description": "no type given"},
		},
	}
	args := map[string]string{"path": "a.txt", "count": "3", "recursive": "true", "untyped": "anything"}
	if err := validateInputSchema(schema, args); err != nil {
		t.Fatalf("validateInputSchema(well-formed) error = %v, want nil", err)
	}
	// An absent optional key must stay absent-and-fine.
	if err := validateInputSchema(map[string]any{"properties": map[string]any{}}, map[string]string{}); err != nil {
		t.Fatalf("validateInputSchema(no required key) error = %v, want nil", err)
	}
}
