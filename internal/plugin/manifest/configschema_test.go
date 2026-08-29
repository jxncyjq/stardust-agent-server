package manifest

import (
	"encoding/json"
	"strings"
	"testing"
)

// pluginWith builds a minimal valid plugin.json with extra top-level fields
// spliced in, in the same shape the tests in manifest_test.go build theirs.
// extra must end with a comma when non-empty.
func pluginWith(extra string) []byte {
	return []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		` + extra + `
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "t", "group": "g", "timeout_ms": 1000}]
	}`)
}

func TestParsePluginAcceptsAConfigSchema(t *testing.T) {
	pm, err := ParsePlugin(pluginWith(
		`"config_schema": {"type":"object","properties":{"endpoint":{"type":"string"}},"required":["endpoint"]},`))
	if err != nil {
		t.Fatalf("ParsePlugin with a config_schema error = %v, want nil", err)
	}
	if len(pm.ConfigSchema) == 0 {
		t.Error("ConfigSchema is empty, want the document from plugin.json")
	}
}

// TestParsePluginWithoutAConfigSchemaIsUnchanged is the backward-compatibility
// pin: every plugin written before this field existed declares nothing, and
// must keep parsing exactly as it did.
func TestParsePluginWithoutAConfigSchemaIsUnchanged(t *testing.T) {
	pm, err := ParsePlugin(pluginWith(""))
	if err != nil {
		t.Fatalf("ParsePlugin without a config_schema error = %v, want nil", err)
	}
	if pm.ConfigSchema != nil {
		t.Errorf("ConfigSchema = %s, want nil when plugin.json declares none", pm.ConfigSchema)
	}
}

// TestParsePluginRefusesAnUnsupportedSchemaKeyword: ignoring a keyword this
// package does not implement would let an author believe a constraint is in
// force when nothing checks it.
func TestParsePluginRefusesAnUnsupportedSchemaKeyword(t *testing.T) {
	_, err := ParsePlugin(pluginWith(
		`"config_schema": {"type":"object","patternProperties":{"^x":{"type":"string"}}},`))
	requireErrorContains(t, err, "patternProperties")
}

func TestParsePluginRefusesASchemaWithAnUnknownType(t *testing.T) {
	_, err := ParsePlugin(pluginWith(`"config_schema": {"type":"tuple"},`))
	requireErrorContains(t, err, "tuple")
}

func TestParsePluginRefusesARequiredFieldThatIsNotDeclared(t *testing.T) {
	_, err := ParsePlugin(pluginWith(
		`"config_schema": {"type":"object","properties":{"a":{"type":"string"}},"required":["b"]},`))
	requireErrorContains(t, err, `"b"`)
}

func TestParsePluginRefusesPropertiesOnANonObject(t *testing.T) {
	_, err := ParsePlugin(pluginWith(
		`"config_schema": {"type":"string","properties":{"a":{"type":"string"}}},`))
	requireErrorContains(t, err, "properties")
}

// TestParsePluginRefusesASchemaNestedTooDeep bounds the recursion: this
// validator is fed a document that arrived with untrusted plugin code.
func TestParsePluginRefusesASchemaNestedTooDeep(t *testing.T) {
	schema := `{"type":"string"}`
	for range configSchemaMaxDepth + 1 {
		schema = `{"type":"object","properties":{"a":` + schema + `}}`
	}
	_, err := ParsePlugin(pluginWith(`"config_schema": ` + schema + `,`))
	requireErrorContains(t, err, "nested")
}

// validateWith parses a schema and runs a config document through it.
func validateWith(t *testing.T, schema, config string) error {
	t.Helper()

	pm, err := ParsePlugin(pluginWith(`"config_schema": ` + schema + `,`))
	if err != nil {
		t.Fatalf("ParsePlugin with schema %s error = %v, want nil", schema, err)
	}
	return ValidateEntryConfig(pm, json.RawMessage(config))
}

func TestValidateEntryConfigAcceptsAConformingDocument(t *testing.T) {
	err := validateWith(t,
		`{"type":"object","properties":{"endpoint":{"type":"string"},"retries":{"type":"integer"}},"required":["endpoint"]}`,
		`{"endpoint":"https://example.test","retries":3}`)
	if err != nil {
		t.Errorf("ValidateEntryConfig on a conforming document = %v, want nil", err)
	}
}

func TestValidateEntryConfigNamesTheFieldWithTheWrongType(t *testing.T) {
	err := validateWith(t, `{"type":"object","properties":{"retries":{"type":"integer"}}}`, `{"retries":"3"}`)
	if err == nil {
		t.Fatal("a string where an integer is declared = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "retries") {
		t.Errorf("error = %v, want it to name the field", err)
	}
}

// TestValidateEntryConfigRejectsAFractionForAnInteger is why numbers are read
// as json.Number: decoding into float64 would make 1.5 and 1 the same kind of
// thing, and "retries: 1.5" is exactly the mistake worth catching.
func TestValidateEntryConfigRejectsAFractionForAnInteger(t *testing.T) {
	err := validateWith(t, `{"type":"object","properties":{"retries":{"type":"integer"}}}`, `{"retries":1.5}`)
	if err == nil {
		t.Fatal("1.5 where an integer is declared = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "retries") {
		t.Errorf("error = %v, want it to name the field", err)
	}
}

func TestValidateEntryConfigNamesAMissingRequiredField(t *testing.T) {
	err := validateWith(t,
		`{"type":"object","properties":{"endpoint":{"type":"string"}},"required":["endpoint"]}`, `{}`)
	if err == nil {
		t.Fatal("a missing required field = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("error = %v, want it to name the missing field", err)
	}
}

// TestValidateEntryConfigRefusesAnUndeclaredField: a typo'd key that is
// silently ignored looks exactly like a setting that did not take effect.
func TestValidateEntryConfigRefusesAnUndeclaredField(t *testing.T) {
	err := validateWith(t, `{"type":"object","properties":{"endpoint":{"type":"string"}}}`, `{"endpiont":"x"}`)
	if err == nil {
		t.Fatal("an undeclared field = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "endpiont") {
		t.Errorf("error = %v, want it to name the offending key", err)
	}
}

func TestValidateEntryConfigAllowsUndeclaredFieldsWhenTheSchemaSaysSo(t *testing.T) {
	err := validateWith(t, `{"type":"object","properties":{},"additional_properties":true}`, `{"anything":1}`)
	if err != nil {
		t.Errorf("ValidateEntryConfig with additional_properties=true = %v, want nil", err)
	}
}

func TestValidateEntryConfigChecksEnums(t *testing.T) {
	schema := `{"type":"object","properties":{"mode":{"type":"string","enum":["fast","safe"]}}}`
	if err := validateWith(t, schema, `{"mode":"safe"}`); err != nil {
		t.Errorf("a declared enum value = %v, want nil", err)
	}
	err := validateWith(t, schema, `{"mode":"reckless"}`)
	if err == nil {
		t.Fatal("a value outside the enum = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "reckless") {
		t.Errorf("error = %v, want it to name the offending value", err)
	}
}

func TestValidateEntryConfigChecksArrayItems(t *testing.T) {
	schema := `{"type":"object","properties":{"hosts":{"type":"array","items":{"type":"string"}}}}`
	if err := validateWith(t, schema, `{"hosts":["a","b"]}`); err != nil {
		t.Errorf("a conforming array = %v, want nil", err)
	}
	err := validateWith(t, schema, `{"hosts":["a",2]}`)
	if err == nil {
		t.Fatal("a number inside a string array = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "hosts[1]") {
		t.Errorf("error = %v, want it to name the offending index", err)
	}
}

func TestValidateEntryConfigChecksNestedObjects(t *testing.T) {
	schema := `{"type":"object","properties":{"auth":{"type":"object","properties":{"token":{"type":"string"}},"required":["token"]}}}`
	if err := validateWith(t, schema, `{"auth":{"token":"t"}}`); err != nil {
		t.Errorf("a conforming nested object = %v, want nil", err)
	}
	err := validateWith(t, schema, `{"auth":{}}`)
	if err == nil {
		t.Fatal("a nested object missing a required field = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "auth.token") {
		t.Errorf("error = %v, want it to name the nested path", err)
	}
}

// TestValidateEntryConfigWithoutASchemaAcceptsAnything is the other half of the
// backward-compatibility pin: a plugin that declares no schema keeps getting
// its configuration passed through unchecked, exactly as before.
func TestValidateEntryConfigWithoutASchemaAcceptsAnything(t *testing.T) {
	if err := ValidateEntryConfig(PluginManifest{}, json.RawMessage(`{"whatever":[1,2]}`)); err != nil {
		t.Errorf("ValidateEntryConfig with no schema = %v, want nil", err)
	}
}

// TestValidateEntryConfigTreatsAnAbsentConfigAsAnEmptyObject: an entry with no
// "config" key must still fail a schema that requires a field — otherwise the
// way to bypass a required setting would be to omit the whole block.
func TestValidateEntryConfigTreatsAnAbsentConfigAsAnEmptyObject(t *testing.T) {
	pm, err := ParsePlugin(pluginWith(
		`"config_schema": {"type":"object","properties":{"endpoint":{"type":"string"}},"required":["endpoint"]},`))
	if err != nil {
		t.Fatalf("ParsePlugin error = %v, want nil", err)
	}
	if err := ValidateEntryConfig(pm, nil); err == nil {
		t.Fatal("a required field against an absent config = nil error, want a refusal")
	}
}

func TestValidateEntryConfigRefusesAConfigThatIsNotAnObject(t *testing.T) {
	err := validateWith(t, `{"type":"object","properties":{}}`, `[1,2]`)
	if err == nil {
		t.Fatal("an array config against an object schema = nil error, want a refusal")
	}
}

func TestValidateEntryConfigRefusesUndecodableJSON(t *testing.T) {
	err := validateWith(t, `{"type":"object","properties":{}}`, `{not json`)
	if err == nil {
		t.Fatal("an undecodable config = nil error, want a refusal")
	}
}

// TestParsePluginAcceptsAMarshalledManifestWithoutASchema is a regression pin,
// not a hypothetical: adding ConfigSchema without ",omitempty" made every
// caller that builds a plugin.json by marshalling this struct emit
// "config_schema": null, which ParsePlugin then refused as a schema with no
// type. Two test suites went red at once.
func TestParsePluginAcceptsAMarshalledManifestWithoutASchema(t *testing.T) {
	data, err := json.Marshal(PluginManifest{
		Name: "p", Version: "1.0.0", ABI: 1, SHA256: validSHA256,
		Limits: Limits{MaxMemoryPages: 1, MaxInstances: 1},
		Tools:  []ToolDecl{{Name: "t", Group: "g", TimeoutMs: 1000}},
	})
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}
	if _, err := ParsePlugin(data); err != nil {
		t.Fatalf("ParsePlugin on a marshalled manifest error = %v, want nil (round-trip must hold)", err)
	}
}

// TestParsePluginTreatsAnExplicitNullSchemaAsAbsent covers the hand-written
// spelling of the same thing.
func TestParsePluginTreatsAnExplicitNullSchemaAsAbsent(t *testing.T) {
	pm, err := ParsePlugin(pluginWith(`"config_schema": null,`))
	if err != nil {
		t.Fatalf("ParsePlugin with a null config_schema error = %v, want nil", err)
	}
	if err := ValidateEntryConfig(pm, json.RawMessage(`{"anything":1}`)); err != nil {
		t.Errorf("ValidateEntryConfig with a null schema = %v, want nil: null declares nothing", err)
	}
}
