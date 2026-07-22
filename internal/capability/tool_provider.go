package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/tool"
)

// ErrNilRegistry reports that a ToolProvider was constructed without a
// registry. This is not ErrUnknownCapability: it does not mean "this
// provider legitimately has no such capability," it means the provider
// itself was never wired up with a registry to begin with -- an assembly
// bug in the caller, not a fact about what tools exist. Nothing in
// NewToolProvider's contract makes a nil registry a legal optional state, so
// Entries and Detail both fail loud with this error rather than reporting an
// empty catalog or a plausible-looking "not found".
var ErrNilRegistry = errors.New("tool provider: registry is nil")

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

// Entries lists the registry's tools as catalog lines. It returns
// ErrNilRegistry if the provider was constructed without a registry.
func (p *ToolProvider) Entries(context.Context) ([]Entry, error) {
	if p.registry == nil {
		return nil, ErrNilRegistry
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
// natively. It returns ErrUnknownCapability for a name this provider does not
// offer, and ErrNilRegistry if the provider was constructed without a
// registry at all (a distinct failure: not "this name is unknown" but "this
// provider was never wired up").
func (p *ToolProvider) Detail(_ context.Context, name string) (string, error) {
	if p.registry == nil {
		return "", fmt.Errorf("tool %q: %w", name, ErrNilRegistry)
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

// summaryTruncationMarker is appended when summarize cuts a description
// short, so the reader can tell the summary is a prefix rather than the
// whole thing.
const summaryTruncationMarker = "..."

// summarize reduces a tool description to a one-line preview bounded by
// MaxSummaryChars. Tool descriptions are written for the model that is about
// to call the tool and run long; the catalog only needs enough to decide
// whether to load the whole thing.
//
// It deliberately does not hunt for a "first sentence" by splitting on the
// first '.'/'。' it finds: built-in tool descriptions interpolate a runtime
// workspace root (see internal/tool/builtin.go), and a real filesystem path
// routinely contains a '.' that is not a sentence boundary (a user directory
// like "first.last", a dotfile, a versioned segment) -- splitting there
// fractures the summary mid-path instead of at the end of a sentence. The
// only boundary treated as structural is a newline, since a description is
// never expected to embed one as literal data. Truncation counts runes, so
// it never cuts a multi-byte character in half, and a cut description gets
// summaryTruncationMarker appended so it reads as "cut short", not as a
// sentence that simply stopped.
func summarize(description string) string {
	text := strings.TrimSpace(description)
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = strings.TrimSpace(text[:idx])
	}
	runes := []rune(text)
	if len(runes) <= MaxSummaryChars {
		return text
	}
	markerLen := len([]rune(summaryTruncationMarker))
	cut := MaxSummaryChars - markerLen
	if cut < 0 {
		cut = 0
	}
	return string(runes[:cut]) + summaryTruncationMarker
}
