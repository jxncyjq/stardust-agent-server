package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestRegistrySubsetExposesOnlyNamedTools(t *testing.T) {
	registry := NewRegistry(nil, nil, nil)
	noop := HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Success: true}, nil
	})
	registry.RegisterDescriptor(Descriptor{Name: "read_file"}, noop)
	registry.RegisterDescriptor(Descriptor{Name: "write_file"}, noop)
	registry.RegisterDescriptor(Descriptor{Name: "delegate_task"}, noop)

	sub := registry.Subset("read_file", "write_file", "ghost")
	names := map[string]bool{}
	for _, d := range sub.Descriptors() {
		names[d.Name] = true
	}
	if len(names) != 2 || !names["read_file"] || !names["write_file"] {
		t.Fatalf("subset descriptors = %v, want read_file+write_file only", names)
	}
	// An excluded tool is unreachable through the subset registry.
	_, err := sub.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "delegate_task"})
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Execute(excluded) error = %v, want ErrToolNotFound", err)
	}
}
