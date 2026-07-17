package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/quality"
)

func TestRegistrySanitizesToolResult(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(
		NewStaticPolicy(DecisionAllow),
		NewRolePermissionEnforcer(map[string]bool{"developer:echo": true}),
		NoopGuardrails{},
	).WithOutputSanitizer(quality.NewOutputSanitizer())
	registry.Register("echo", HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: "call-1", Success: true, Output: "ok\x1b[31m red\x1b[0m\nnext"}, nil
	}))

	result, err := registry.Execute(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.ToolCall{
		ID:   "call-1",
		Name: "echo",
	})
	if err != nil {
		t.Fatalf("Execute(echo) error = %v, want nil", err)
	}
	if strings.Contains(result.Output, "\x1b") {
		t.Fatalf("Execute(echo) output = %q, want no ANSI escape", result.Output)
	}
	if strings.Contains(result.Output, "\n") {
		t.Fatalf("Execute(echo) output = %q, want no newline", result.Output)
	}
}
