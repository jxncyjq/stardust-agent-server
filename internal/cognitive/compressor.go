package cognitive

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/port"
)

type CompressionStrategy int

const (
	CompressionStrategyNone CompressionStrategy = iota
	CompressionStrategyTrimProtect
	CompressionStrategyFourLayer
	CompressionStrategyCheckpoint
)

type Message struct {
	Role      string
	Kind      string
	Content   string
	CreatedAt time.Time
}

type MessageHistory struct {
	Messages []Message
}

type CompressionReport struct {
	Strategy      CompressionStrategy
	LayersApplied []int
	TokensBefore  int
	TokensAfter   int
	Summary       string
	CompressedAt  time.Time
}

type CheckpointResult struct {
	CycleIndex  int
	Summary     string
	TokensSaved int
}

type ContextCompressorConfig struct {
	TokenLimit         int
	ProtectedHead      int
	ProtectedTail      int
	ToolResultMaxChars int
	Summarizer         port.MaasInferenceClient
	// Counter estimates token lengths for threshold decisions. It is an
	// explicit optional slot: when nil, NewContextCompressor installs a
	// CJK-aware default. This is a contract-declared default, not a fallback.
	Counter port.TokenCounter
}

type ContextCompressor struct {
	cfg ContextCompressorConfig
}

// DefaultContextCompressorConfig returns compression thresholds calibrated for
// the CJK-aware token counter (the default Counter). The whitespace counter that
// preceded it under-counted CJK text by ~10x, so the same TokenLimit triggered
// far too late on Chinese histories; these values assume near-BPE token counts.
// TokenLimit ~8k keeps a working history well inside typical model context while
// leaving room for the response; ProtectedHead/Tail keep the task framing and the
// latest exchange verbatim; ToolResultMaxChars trims oversized tool dumps before
// summarizing. summarizer may be nil, in which case compression degrades to
// trim-and-protect (no LLM summary layer).
func DefaultContextCompressorConfig(summarizer port.MaasInferenceClient) ContextCompressorConfig {
	return ContextCompressorConfig{
		TokenLimit:         8000,
		ProtectedHead:      2,
		ProtectedTail:      2,
		ToolResultMaxChars: 4000,
		Summarizer:         summarizer,
		Counter:            NewCJKTokenCounter(),
	}
}

func NewContextCompressor(cfg ContextCompressorConfig) *ContextCompressor {
	if cfg.ProtectedHead < 0 {
		cfg.ProtectedHead = 0
	}
	if cfg.ProtectedTail < 0 {
		cfg.ProtectedTail = 0
	}
	if cfg.Counter == nil {
		cfg.Counter = NewCJKTokenCounter()
	}
	return &ContextCompressor{cfg: cfg}
}

func (c *ContextCompressor) Compress(ctx context.Context, text string) (CompressionResult, error) {
	history := MessageHistory{Messages: []Message{{Role: "system", Kind: "context", Content: text}}}
	compressed, report, err := c.CompressHistory(ctx, history)
	if err != nil {
		return CompressionResult{}, err
	}
	return CompressionResult{
		Text:       renderMessages(compressed.Messages),
		Compressed: report.TokensAfter < report.TokensBefore,
	}, nil
}

func (c *ContextCompressor) CompressHistory(ctx context.Context, history MessageHistory) (MessageHistory, CompressionReport, error) {
	if err := ctx.Err(); err != nil {
		return MessageHistory{}, CompressionReport{}, err
	}
	messages := copyMessages(history.Messages)
	before := c.countHistoryTokens(messages)
	report := CompressionReport{
		Strategy:      CompressionStrategyNone,
		TokensBefore:  before,
		TokensAfter:   before,
		CompressedAt:  time.Now().UTC(),
		LayersApplied: []int{},
	}
	if c.cfg.TokenLimit <= 0 || before <= c.cfg.TokenLimit {
		return MessageHistory{Messages: messages}, report, nil
	}

	if c.trimToolResults(messages) {
		report.LayersApplied = append(report.LayersApplied, 1)
	}

	head, middle, tail := splitProtected(messages, c.cfg.ProtectedHead, c.cfg.ProtectedTail)
	report.LayersApplied = append(report.LayersApplied, 2)
	report.Strategy = CompressionStrategyTrimProtect
	if c.cfg.Summarizer == nil {
		compressed := append(append([]Message{}, head...), tail...)
		report.TokensAfter = c.countHistoryTokens(compressed)
		return MessageHistory{Messages: compressed}, report, nil
	}

	summary, err := c.summarize(ctx, "Summarize middle context for continued task execution.", middle)
	if err != nil {
		return MessageHistory{}, CompressionReport{}, fmt.Errorf("summarize context: %w", err)
	}
	report.LayersApplied = append(report.LayersApplied, 3)
	report.Strategy = CompressionStrategyFourLayer
	report.Summary = summary

	summaryMessage := Message{
		Role:      "system",
		Kind:      "summary",
		Content:   summary,
		CreatedAt: time.Now().UTC(),
	}
	compressed := append(append([]Message{}, head...), summaryMessage)
	compressed = append(compressed, tail...)
	report.TokensAfter = c.countHistoryTokens(compressed)
	return MessageHistory{Messages: compressed}, report, nil
}

func (c *ContextCompressor) ForceCheckpoint(ctx context.Context, cycleIndex int, history MessageHistory) (CheckpointResult, error) {
	if err := ctx.Err(); err != nil {
		return CheckpointResult{}, err
	}
	summary, err := c.summarize(ctx, fmt.Sprintf("Create checkpoint for cycle %d.", cycleIndex), history.Messages)
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("create checkpoint: %w", err)
	}
	tokensBefore := c.countHistoryTokens(history.Messages)
	tokensAfter := c.countTokens(summary)
	tokensSaved := tokensBefore - tokensAfter
	if tokensSaved <= 0 {
		tokensSaved = 1
	}
	return CheckpointResult{
		CycleIndex:  cycleIndex,
		Summary:     summary,
		TokensSaved: tokensSaved,
	}, nil
}

func (c *ContextCompressor) trimToolResults(messages []Message) bool {
	limit := c.cfg.ToolResultMaxChars
	if limit <= 0 {
		return false
	}
	trimmed := false
	for i := range messages {
		if messages[i].Kind != "tool_result" || len(messages[i].Content) <= limit {
			continue
		}
		messages[i].Content = messages[i].Content[:limit] + "\n[tool_result_trimmed]"
		trimmed = true
	}
	return trimmed
}

func (c *ContextCompressor) summarize(ctx context.Context, instruction string, messages []Message) (string, error) {
	if c.cfg.Summarizer == nil {
		return "", fmt.Errorf("summarizer unavailable")
	}
	resp, err := c.cfg.Summarizer.Generate(ctx, port.InferenceRequest{
		RequestID: "context-compressor",
		Prompt:    instruction + "\n\n" + renderMessages(messages),
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text), nil
}

func splitProtected(messages []Message, headCount, tailCount int) ([]Message, []Message, []Message) {
	if headCount > len(messages) {
		headCount = len(messages)
	}
	remaining := len(messages) - headCount
	if tailCount > remaining {
		tailCount = remaining
	}
	head := copyMessages(messages[:headCount])
	middle := copyMessages(messages[headCount : len(messages)-tailCount])
	tail := copyMessages(messages[len(messages)-tailCount:])
	return head, middle, tail
}

func renderMessages(messages []Message) string {
	var b strings.Builder
	for _, message := range messages {
		if message.Role != "" {
			b.WriteString(message.Role)
			b.WriteString(": ")
		}
		if message.Kind != "" {
			b.WriteString("[")
			b.WriteString(message.Kind)
			b.WriteString("] ")
		}
		b.WriteString(message.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func copyMessages(messages []Message) []Message {
	if len(messages) == 0 {
		return nil
	}
	copied := make([]Message, len(messages))
	copy(copied, messages)
	return copied
}

func (c *ContextCompressor) countHistoryTokens(messages []Message) int {
	return c.countTokens(renderMessages(messages))
}

func (c *ContextCompressor) countTokens(text string) int {
	return c.cfg.Counter.Count(text)
}
