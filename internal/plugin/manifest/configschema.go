package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// configSchemaMaxDepth bounds how deeply a config_schema may nest.
//
// Five levels is more than any plugin configuration has needed, and the bound
// exists because the validator below is RECURSIVE and is fed a document that
// arrived with untrusted plugin code: an unbounded recursion over attacker-
// chosen input is a stack overflow waiting to be written, and a wasm plugin's
// manifest is exactly attacker-chosen input.
const configSchemaMaxDepth = 5

// Supported schema types. Everything outside this list is refused by name when
// the manifest is parsed, rather than ignored: a keyword nobody implements is
// a constraint the author believes is in force while nothing checks it.
const (
	schemaTypeObject  = "object"
	schemaTypeString  = "string"
	schemaTypeNumber  = "number"
	schemaTypeInteger = "integer"
	schemaTypeBoolean = "boolean"
	schemaTypeArray   = "array"
)

// ConfigSchema is the subset of JSON Schema a plugin may use to describe the
// deployment-side configuration it expects (PluginManifest.ConfigSchema).
//
// It is a SUBSET on purpose. A complete implementation means a third-party
// dependency, and every byte of that dependency travels into a deployment
// alongside untrusted plugin code; the shapes plugin configuration actually
// takes — an object of scalars, sometimes one level of nesting, sometimes a
// list of strings — need none of $ref, allOf, if/then or pattern matching.
//
// Supported keywords:
//
//	type                   object | string | number | integer | boolean | array
//	properties             on an object: field name -> sub-schema
//	required               on an object: field names, each of which must appear in properties
//	additional_properties  on an object: default FALSE (an undeclared field is an error)
//	items                  on an array: the element sub-schema
//	enum                   on a scalar: the allowed values
//	description            anywhere: for humans only
//
// additional_properties defaulting to false is the one choice worth stating
// twice: a mistyped key that is silently ignored is indistinguishable from a
// setting that did not take effect, and an operator chasing that has nothing
// to go on.
type ConfigSchema struct {
	typeName             string
	description          string
	properties           map[string]*ConfigSchema
	propertyOrder        []string
	required             []string
	additionalProperties bool
	items                *ConfigSchema
	enum                 []json.RawMessage
}

// rawConfigSchema is the wire shape. Unknown fields are refused by the decoder
// (see parseConfigSchemaNode), which is how an unsupported keyword becomes a
// named error instead of a silent no-op.
type rawConfigSchema struct {
	Type                 string                     `json:"type"`
	Description          string                     `json:"description"`
	Properties           map[string]json.RawMessage `json:"properties"`
	Required             []string                   `json:"required"`
	AdditionalProperties *bool                      `json:"additional_properties"`
	Items                json.RawMessage            `json:"items"`
	Enum                 []json.RawMessage          `json:"enum"`
}

// ParseConfigSchema decodes and validates a config_schema document.
//
// It refuses, naming the offender in every case:
//
//   - any keyword outside the supported set, at any depth;
//   - a type outside the supported list;
//   - properties or required on something that is not an object, and items on
//     something that is not an array — a constraint attached where nothing
//     will ever read it is a mistake, not a harmless extra;
//   - a required name that does not appear in properties;
//   - nesting deeper than configSchemaMaxDepth.
func ParseConfigSchema(raw json.RawMessage) (*ConfigSchema, error) {
	return parseConfigSchemaNode(raw, "", 1)
}

func parseConfigSchemaNode(raw json.RawMessage, path string, depth int) (*ConfigSchema, error) {
	if depth > configSchemaMaxDepth {
		return nil, fmt.Errorf("%s is nested deeper than %d levels", schemaWhere(path), configSchemaMaxDepth)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var node rawConfigSchema
	if err := decoder.Decode(&node); err != nil {
		return nil, fmt.Errorf("%s: %w", schemaWhere(path), err)
	}

	switch node.Type {
	case schemaTypeObject, schemaTypeString, schemaTypeNumber, schemaTypeInteger, schemaTypeBoolean, schemaTypeArray:
	case "":
		return nil, fmt.Errorf("%s has no %q", schemaWhere(path), "type")
	default:
		return nil, fmt.Errorf("%s has unsupported type %q; supported: object, string, number, integer, boolean, array",
			schemaWhere(path), node.Type)
	}

	schema := &ConfigSchema{
		typeName:    node.Type,
		description: node.Description,
		enum:        node.Enum,
		// Absent means false: an undeclared field is an error unless the author
		// says otherwise. See the type's doc comment.
		additionalProperties: node.AdditionalProperties != nil && *node.AdditionalProperties,
	}

	if node.Type != schemaTypeObject {
		if len(node.Properties) > 0 {
			return nil, fmt.Errorf("%s declares %q but its type is %q, so nothing would ever read them",
				schemaWhere(path), "properties", node.Type)
		}
		if len(node.Required) > 0 {
			return nil, fmt.Errorf("%s declares %q but its type is %q", schemaWhere(path), "required", node.Type)
		}
		if node.AdditionalProperties != nil {
			return nil, fmt.Errorf("%s declares %q but its type is %q",
				schemaWhere(path), "additional_properties", node.Type)
		}
	}
	if node.Type != schemaTypeArray && len(node.Items) > 0 {
		return nil, fmt.Errorf("%s declares %q but its type is %q", schemaWhere(path), "items", node.Type)
	}

	if len(node.Properties) > 0 {
		schema.properties = make(map[string]*ConfigSchema, len(node.Properties))
		// Sorted so an error about "the first bad property" is the same one on
		// every run: map iteration order would make a failing build report a
		// different field each time.
		for _, name := range sortedKeys(node.Properties) {
			child, err := parseConfigSchemaNode(node.Properties[name], joinSchemaPath(path, name), depth+1)
			if err != nil {
				return nil, err
			}
			schema.properties[name] = child
			schema.propertyOrder = append(schema.propertyOrder, name)
		}
	}

	for _, name := range node.Required {
		if _, declared := schema.properties[name]; !declared {
			return nil, fmt.Errorf("%s requires %q, which it does not declare in %q",
				schemaWhere(path), name, "properties")
		}
	}
	schema.required = node.Required

	if len(node.Items) > 0 {
		items, err := parseConfigSchemaNode(node.Items, joinSchemaPath(path, "[]"), depth+1)
		if err != nil {
			return nil, err
		}
		schema.items = items
	}
	return schema, nil
}

// ValidateEntryConfig checks a deployment entry's config against the schema the
// plugin declared.
//
// A plugin that declares no schema accepts anything, which is what every
// plugin written before config_schema existed does and must keep doing: its
// configuration is passed to the guest verbatim, unexamined, exactly as
// before.
//
// An ABSENT config is validated as an empty object rather than skipped. The
// alternative would make "omit the whole config block" a way around a required
// field, which is precisely the mistake this function exists to catch.
func ValidateEntryConfig(pm PluginManifest, config json.RawMessage) error {
	if !hasConfigSchema(pm.ConfigSchema) {
		return nil
	}
	schema, err := ParseConfigSchema(pm.ConfigSchema)
	if err != nil {
		// Unreachable through ParsePlugin, which validates the schema when the
		// manifest is read — and still not assumed: a caller assembling a
		// PluginManifest by hand gets a named error rather than a silent pass.
		return fmt.Errorf("plugin %q: config_schema: %w", pm.Name, err)
	}
	document := config
	if len(bytes.TrimSpace(document)) == 0 {
		document = json.RawMessage("{}")
	}

	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("plugin %q: config is not valid JSON: %w", pm.Name, err)
	}
	if err := schema.validate(value, "config"); err != nil {
		return fmt.Errorf("plugin %q: %w", pm.Name, err)
	}
	return nil
}

// validate checks one decoded value against this schema node.
//
// Numbers arrive as json.Number because the caller decodes with UseNumber:
// decoding into float64 would erase the difference between 1 and 1.5, and
// "retries: 1.5" is exactly the kind of mistake worth catching.
func (s *ConfigSchema) validate(value any, path string) error {
	switch s.typeName {
	case schemaTypeObject:
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want an object, got %s", path, jsonKindOf(value))
		}
		for _, name := range s.required {
			if _, present := object[name]; !present {
				return fmt.Errorf("%s: missing required field %q", joinConfigPath(path, name), name)
			}
		}
		if !s.additionalProperties {
			for _, name := range sortedAnyKeys(object) {
				if _, declared := s.properties[name]; !declared {
					return fmt.Errorf("%s: unknown field %q; the plugin declares %s",
						path, name, declaredList(s.propertyOrder))
				}
			}
		}
		for _, name := range s.propertyOrder {
			child, present := object[name]
			if !present {
				continue
			}
			if err := s.properties[name].validate(child, joinConfigPath(path, name)); err != nil {
				return err
			}
		}
		return nil

	case schemaTypeArray:
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: want an array, got %s", path, jsonKindOf(value))
		}
		if s.items == nil {
			return nil
		}
		for i, item := range items {
			if err := s.items.validate(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil

	case schemaTypeString:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: want a string, got %s", path, jsonKindOf(value))
		}
		return s.checkEnum(value, path)

	case schemaTypeBoolean:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: want a boolean, got %s", path, jsonKindOf(value))
		}
		return s.checkEnum(value, path)

	case schemaTypeNumber, schemaTypeInteger:
		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("%s: want a %s, got %s", path, s.typeName, jsonKindOf(value))
		}
		if s.typeName == schemaTypeInteger {
			if _, err := number.Int64(); err != nil {
				return fmt.Errorf("%s: want an integer, got %s", path, number.String())
			}
		} else if _, err := number.Float64(); err != nil {
			return fmt.Errorf("%s: want a number, got %s", path, number.String())
		}
		return s.checkEnum(value, path)
	}
	// Unreachable: parseConfigSchemaNode refuses every other type name. Saying
	// so is cheaper than a silent "accept anything" if that ever changes.
	return fmt.Errorf("%s: schema declares unsupported type %q", path, s.typeName)
}

// checkEnum enforces the declared value set, comparing the JSON rendering of
// the value against the raw enum entries — which is what makes 1 and 1.0
// compare as written rather than as float64 round-trips.
func (s *ConfigSchema) checkEnum(value any, path string) error {
	if len(s.enum) == 0 {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s: cannot compare against the declared values: %w", path, err)
	}
	allowed := make([]string, 0, len(s.enum))
	for _, candidate := range s.enum {
		trimmed := strings.TrimSpace(string(candidate))
		allowed = append(allowed, trimmed)
		if trimmed == string(encoded) {
			return nil
		}
	}
	return fmt.Errorf("%s: %s is not one of the declared values %s",
		path, string(encoded), "["+strings.Join(allowed, " ")+"]")
}

// jsonKindOf names what a decoded value actually is, for error messages.
func jsonKindOf(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func schemaWhere(path string) string {
	if path == "" {
		return "config_schema"
	}
	return "config_schema at " + path
}

func joinSchemaPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func joinConfigPath(path, name string) string {
	return path + "." + name
}

func declaredList(names []string) string {
	if len(names) == 0 {
		return "no fields at all"
	}
	return "[" + strings.Join(names, " ") + "]"
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func sortedAnyKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// hasConfigSchema reports whether a manifest actually declares a schema.
//
// A literal JSON null counts as ABSENT, not as an empty schema. Two things
// produce one: a hand-written plugin.json with "config_schema": null, and a
// PluginManifest marshalled without the ",omitempty" tag — which is how this
// was first caught, when adding the field made every test that builds a
// package by marshalling the struct fail to parse it back.
func hasConfigSchema(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}
