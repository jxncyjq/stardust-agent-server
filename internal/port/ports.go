package port

import (
	"context"

	"github.com/stardust/legion-agent/internal/domain"
)

type InferenceRequest struct {
	RequestID string
	Prompt    string
	Tools     []InferenceTool
	// Images carries optional multimodal inputs as data-URI strings. When
	// non-empty the inference client must emit them in the model's vision
	// format; when empty the request stays text-only and byte-for-byte
	// backward compatible.
	Images []string
	// StablePrefixLen marks how many leading runes of Prompt are stable across
	// calls in the same task (system + task framing). Adapters that support
	// provider prompt caching (e.g. Anthropic cache_control) place a cache
	// breakpoint at this boundary. Zero means "no known stable prefix" and is
	// fully backward compatible — adapters treat the whole prompt as volatile.
	StablePrefixLen int
}

type InferenceResponse struct {
	Text             string            `json:"text"`
	ReasoningSummary string            `json:"reasoning_summary,omitempty"`
	ToolCalls        []domain.ToolCall `json:"tool_calls,omitempty"`
	PromptTokens     int               `json:"prompt_tokens,omitempty"`
	CompletionTokens int               `json:"completion_tokens,omitempty"`
	// CachedTokens is the subset of PromptTokens served from the provider's
	// prompt cache. Contract-optional: providers that omit prompt_tokens_details
	// leave it at zero.
	CachedTokens int `json:"cached_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

type InferenceTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

type MaasInferenceClient interface {
	Generate(ctx context.Context, req InferenceRequest) (InferenceResponse, error)
}

type EmbeddingProvider interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

type OutputSanitizer interface {
	Label(text string) string
	HTML(text string) string
	YAMLString(text string) string
	MarkdownInline(text string) string
	Truncate(text string, maxLen int) string
}

type EventBus interface {
	Publish(ctx context.Context, event domain.RuntimeEvent) error
	// Events returns the recorded runtime events. Backing stores can fail
	// (a SQLite query error, a closed handle); the error is part of the
	// contract so a read failure surfaces as a failure instead of being
	// indistinguishable from "no events have been published yet".
	Events() ([]domain.RuntimeEvent, error)
}

type AuditLog interface {
	Append(ctx context.Context, event domain.AuditEvent) error
	// Events returns the recorded audit events. As with EventBus.Events, a
	// backing-store failure is reported rather than collapsed into an empty
	// slice — an audit trail that reads as empty because the query failed is
	// worse than one that reads as broken.
	Events() ([]domain.AuditEvent, error)
}
