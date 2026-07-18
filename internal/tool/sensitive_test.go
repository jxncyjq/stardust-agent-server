package tool

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestSafeToolNamesExcludesSensitive(t *testing.T) {
	reg := NewRegistry(
		NewExecutionPolicy(ExecutionPolicyConfig{AutoAllowTools: []string{"reader", "writer"}}),
		PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		NoopGuardrails{},
	)
	reg.RegisterDescriptor(Descriptor{Name: "reader", Sensitive: false}, HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Success: true}, nil
	}))
	reg.RegisterDescriptor(Descriptor{Name: "writer", Sensitive: true}, HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Success: true}, nil
	}))

	safe := reg.SafeToolNames()
	if len(safe) != 1 || safe[0] != "reader" {
		t.Fatalf("SafeToolNames = %v, want [reader]", safe)
	}
}

func TestBuiltinWorkspaceToolsClassification(t *testing.T) {
	// NewReadOnlyWorkspaceRegistry does not register write_file (only
	// registerReadOnlyDescriptors); NewWorkspaceRegistry is the constructor that
	// actually registers all four tools below, so it is used here instead of the
	// brief's suggested NewReadOnlyWorkspaceRegistry.
	reg := NewWorkspaceRegistry(t.TempDir(), nil)
	want := map[string]bool{ // name -> sensitive
		"read_file":      false,
		"search_content": false,
		"list_files":     false,
		"write_file":     true,
	}
	got := map[string]bool{}
	for _, d := range reg.Descriptors() {
		got[d.Name] = d.Sensitive
	}
	for name, sensitive := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("tool %q not registered by NewReadOnlyWorkspaceRegistry", name)
			continue
		}
		if g != sensitive {
			t.Errorf("tool %q Sensitive = %v, want %v", name, g, sensitive)
		}
	}
}
