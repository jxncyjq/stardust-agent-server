package port

import (
	"context"
	"fmt"
	"strings"

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
	// Messages carries a multi-turn exchange (user / assistant-with-tool_calls /
	// tool-result). When non-empty it is authoritative and Prompt must be empty.
	// The tool loop uses it so the model can see the calls it already made —
	// something a re-sent single user message cannot express, and whose absence
	// let a model re-issue the same call indefinitely.
	//
	// Multi-turn requests set no cache breakpoint: the exchange is append-only,
	// so its leading messages are already a stable prefix for providers that do
	// automatic prefix caching.
	Messages []InferenceMessage
}

// Roles accepted in InferenceRequest.Messages. There is deliberately no system
// role: task framing lives in the first user message, exactly as it does in the
// single-turn Prompt contract this extends.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// InferenceMessage is one turn of a multi-turn exchange. Role decides which
// fields carry meaning: Images only on user, ToolCalls only on assistant,
// ToolCallID only on tool — and required there, since an OpenAI-compatible
// provider rejects a tool message it cannot pair with a preceding tool call.
type InferenceMessage struct {
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	Images     []string          `json:"images,omitempty"`
	ToolCalls  []domain.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

// Validate enforces the request's shape before it reaches an adapter. Prompt and
// Messages are mutually exclusive: accepting both would leave which one the
// model actually sees up to each adapter, so a caller that filled the wrong
// field would silently receive an answer to a different question.
func (r InferenceRequest) Validate() error {
	if len(r.Messages) == 0 {
		return nil
	}
	if strings.TrimSpace(r.Prompt) != "" {
		return fmt.Errorf("inference request %s: Prompt and Messages are mutually exclusive", r.RequestID)
	}
	for i, msg := range r.Messages {
		switch msg.Role {
		case RoleUser, RoleAssistant:
		case RoleTool:
			if strings.TrimSpace(msg.ToolCallID) == "" {
				return fmt.Errorf("inference request %s: message %d has role tool without ToolCallID", r.RequestID, i)
			}
		default:
			return fmt.Errorf("inference request %s: message %d has unknown role %q", r.RequestID, i, msg.Role)
		}
	}
	return nil
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
