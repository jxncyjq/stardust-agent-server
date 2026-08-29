package loader

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/manifest"
)

// writeEchoWithConfigSchema writes the echo package with a declared
// config_schema and returns its deployment entry carrying config.
//
// It mirrors harness.writeEcho rather than replacing it: every other test in
// this package needs the schema-less fixture, and that path is exactly what
// the backward-compatibility test below pins.
func (h *harness) writeEchoWithConfigSchema(schema, config string) manifest.Entry {
	h.t.Helper()

	writePackage(h.t, filepath.Join(h.root, "echo"), pkg{
		wasm:         fixtureWasm(h.t, echoWasmFile),
		name:         echoPluginName,
		version:      "1.0.0",
		tools:        []string{echoToolName},
		configSchema: json.RawMessage(schema),
	})
	entry := entryFor(echoPluginName, "echo", nil, echoToolName)
	entry.Config = json.RawMessage(config)
	return entry
}

// TestApplyFailsAnEntryWhoseConfigDoesNotMatchTheSchema is the point of the
// whole feature: a mistyped value is refused at LOAD time, naming the field,
// instead of reaching the guest — which is the layer with the least ability to
// explain it.
func TestApplyFailsAnEntryWhoseConfigDoesNotMatchTheSchema(t *testing.T) {
	h := newHarness(t)
	entry := h.writeEchoWithConfigSchema(
		`{"type":"object","properties":{"endpoint":{"type":"string"}},"required":["endpoint"]}`,
		`{"endpoint":42}`)

	// Apply reports per-entry failures through Status, not by failing the whole
	// convergence — the existing semantics, unchanged here.
	if err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root); err == nil {
		t.Log("Apply returned nil; the per-entry failure is asserted through Status below")
	}

	statuses := h.loader.Status()
	if len(statuses) != 1 {
		t.Fatalf("Status() = %d rows, want 1", len(statuses))
	}
	if statuses[0].State != StateFailed {
		t.Errorf("state = %q, want %q", statuses[0].State, StateFailed)
	}
	if !strings.Contains(statuses[0].LastError, "endpoint") {
		t.Errorf("LastError = %q, want it to name the offending field", statuses[0].LastError)
	}
	if names := h.toolNames(); len(names) != 0 {
		t.Errorf("tools = %v, want none: a plugin whose config was refused must not be activated", names)
	}
}

func TestApplyNamesAMissingRequiredConfigField(t *testing.T) {
	h := newHarness(t)
	entry := h.writeEchoWithConfigSchema(
		`{"type":"object","properties":{"endpoint":{"type":"string"}},"required":["endpoint"]}`,
		`{}`)

	_ = h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)

	statuses := h.loader.Status()
	if len(statuses) != 1 || statuses[0].State != StateFailed {
		t.Fatalf("Status() = %+v, want one failed entry", statuses)
	}
	if !strings.Contains(statuses[0].LastError, "endpoint") {
		t.Errorf("LastError = %q, want it to name the missing field", statuses[0].LastError)
	}
}

func TestApplyMountsAnEntryWhoseConfigMatchesTheSchema(t *testing.T) {
	h := newHarness(t)
	entry := h.writeEchoWithConfigSchema(
		`{"type":"object","properties":{"endpoint":{"type":"string"}},"required":["endpoint"]}`,
		`{"endpoint":"https://example.test"}`)

	h.apply(entry)

	if names := h.toolNames(); len(names) != 1 || names[0] != echoToolName {
		t.Errorf("tools = %v, want the plugin mounted with %s", names, echoToolName)
	}
}

// TestApplyIsUnchangedForAPluginWithoutAConfigSchema is the backward
// compatibility net: every plugin that exists today declares no schema, and
// its config — whatever shape it has — must still be passed through untouched.
func TestApplyIsUnchangedForAPluginWithoutAConfigSchema(t *testing.T) {
	h := newHarness(t)
	entry := h.writeEcho("1.0.0", echoToolName)
	entry.Config = json.RawMessage(`{"anything":[1,2],"nested":{"deep":true}}`)

	h.apply(entry)

	if names := h.toolNames(); len(names) != 1 {
		t.Errorf("tools = %v, want the plugin mounted: a plugin with no schema accepts any config", names)
	}
}
