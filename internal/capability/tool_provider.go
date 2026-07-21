package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/tool"
)

// ToolProvider exposes a tool registry as catalog entries.
//
// It reads whatever registry it is handed rather than a global one, because
// the registry a task may use is already narrowed (Plan mode drops the
// side-effecting tools, per-agent tasks get their own set). A catalog built
// from a wider registry than the caller's would advertise tools that task is
// not allowed to run.
type ToolProvider struct {
	registry *tool.Registry
}

// NewToolProvider returns a Provider backed by registry.
func NewToolProvider(registry *tool.Registry) *ToolProvider {
	return &ToolProvider{registry: registry}
}

// Entries lists the registry's tools as catalog lines.
func (p *ToolProvider) Entries(context.Context) ([]Entry, error) {
	if p.registry == nil {
		return nil, nil
	}
	descriptors := p.registry.Descriptors()
	entries := make([]Entry, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.Group == "" {
			return nil, fmt.Errorf("tool %q declares no catalog group", descriptor.Name)
		}
		entries = append(entries, Entry{
			Name:    descriptor.Name,
			Group:   descriptor.Group,
			Summary: summarize(descriptor.Description),
			Kind:    KindTool,
		})
	}
	return entries, nil
}

// Detail returns the tool's name, description and input schema as JSON -- the
// same three fields the model would have received had the tool been offered
// natively.
func (p *ToolProvider) Detail(_ context.Context, name string) (string, error) {
	if p.registry == nil {
		return "", fmt.Errorf("%w: %s", ErrUnknownCapability, name)
	}
	for _, descriptor := range p.registry.Descriptors() {
		if descriptor.Name != name {
			continue
		}
		encoded, err := json.Marshal(map[string]any{
			"name":         descriptor.Name,
			"description":  descriptor.Description,
			"input_schema": descriptor.InputSchema,
		})
		if err != nil {
			return "", fmt.Errorf("marshal tool %q schema: %w", name, err)
		}
		return string(encoded), nil
	}
	return "", fmt.Errorf("%w: %s", ErrUnknownCapability, name)
}

// summarize reduces a tool description to its first sentence, bounded by
// MaxSummaryChars. Tool descriptions are written for the model that is about
// to call the tool and run long; the catalog only needs enough to decide
// whether to load the whole thing.
func summarize(description string) string {
	text := strings.TrimSpace(description)
	if idx := strings.IndexAny(text, ".。\n"); idx > 0 {
		text = strings.TrimSpace(text[:idx])
	}
	runes := []rune(text)
	if len(runes) > MaxSummaryChars {
		return string(runes[:MaxSummaryChars])
	}
	return text
}
