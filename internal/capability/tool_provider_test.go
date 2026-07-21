package capability_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/tool"
)

// newTestRegistry builds a registry with no policy, enforcer or guardrails.
//
// The brief's reference test constructs this with tool.Guardrails{}, but
// Guardrails is an interface (tool/registry.go), so a composite literal of
// that type does not compile. nil is the equivalent zero value here: Registry
// treats a nil guards field as "no guardrails" (see Registry.Execute's
// `if r.guards != nil` checks), so behavior is unchanged.
func newTestRegistry(t *testing.T, descriptors ...tool.Descriptor) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry(nil, nil, nil)
	for _, descriptor := range descriptors {
		registry.RegisterDescriptor(descriptor, tool.HandlerFunc(
			func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
				return domain.ToolResult{}, nil
			}))
	}
	return registry
}

func TestToolProviderEntriesCarryGroupAndSummary(t *testing.T) {
	t.Parallel()
	registry := newTestRegistry(t, tool.Descriptor{
		Name:        "read_file",
		Group:       "files",
		Description: "Read a UTF-8 text file inside the workspace root.",
		InputSchema: map[string]any{"type": "object"},
	})

	entries, err := capability.NewToolProvider(registry).Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries() error = %v, want nil", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Entries() len = %d, want 1", len(entries))
	}
	if entries[0].Group != "files" {
		t.Errorf("group = %q, want %q", entries[0].Group, "files")
	}
	if entries[0].Kind != capability.KindTool {
		t.Errorf("kind = %v, want KindTool", entries[0].Kind)
	}
	if entries[0].Summary == "" {
		t.Error("summary is empty, want the descriptor's first sentence")
	}
}

func TestToolProviderDetailIsTheSchemaTheModelWouldHaveSeen(t *testing.T) {
	t.Parallel()
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
	}
	registry := newTestRegistry(t, tool.Descriptor{
		Name:        "read_file",
		Group:       "files",
		Description: "Read a file.",
		InputSchema: schema,
	})

	detail, err := capability.NewToolProvider(registry).Detail(context.Background(), "read_file")
	if err != nil {
		t.Fatalf("Detail(read_file) error = %v, want nil", err)
	}
	var decoded struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"input_schema"`
	}
	if err := json.Unmarshal([]byte(detail), &decoded); err != nil {
		t.Fatalf("Detail is not valid JSON: %v (%s)", err, detail)
	}
	if decoded.Name != "read_file" {
		t.Errorf("name = %q, want %q", decoded.Name, "read_file")
	}
	if decoded.InputSchema == nil {
		t.Error("input_schema missing: the model cannot call a tool whose parameters it never sees")
	}
}

func TestToolProviderDetailUnknownName(t *testing.T) {
	t.Parallel()
	registry := newTestRegistry(t)

	_, err := capability.NewToolProvider(registry).Detail(context.Background(), "nope")
	if !errors.Is(err, capability.ErrUnknownCapability) {
		t.Fatalf("Detail(nope) error = %v, want ErrUnknownCapability", err)
	}
}

func TestToolProviderRejectsDescriptorWithoutGroup(t *testing.T) {
	t.Parallel()
	registry := newTestRegistry(t, tool.Descriptor{
		Name:        "ungrouped",
		Description: "No group declared.",
		InputSchema: map[string]any{"type": "object"},
	})

	// 未标注分组的工具无法在目录里落位,这是注册时的疏漏,必须报出来。
	if _, err := capability.NewToolProvider(registry).Entries(context.Background()); err == nil {
		t.Fatal("Entries() error = nil, want an error naming the ungrouped tool")
	}
}
