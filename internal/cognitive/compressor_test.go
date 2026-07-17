package cognitive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/port"
)

func TestContextCompressorFourLayerStrategy(t *testing.T) {
	t.Parallel()

	maas := &recordingInferenceClient{response: "summary: keep the successful plan and final failure"}
	compressor := NewContextCompressor(ContextCompressorConfig{
		TokenLimit:         18,
		ProtectedHead:      1,
		ProtectedTail:      1,
		ToolResultMaxChars: 12,
		Summarizer:         maas,
	})
	history := MessageHistory{
		Messages: []Message{
			{Role: "system", Kind: "instruction", Content: "keep root instruction", CreatedAt: time.Unix(1, 0)},
			{Role: "tool", Kind: "tool_result", Content: "very large tool result " + strings.Repeat("x", 80), CreatedAt: time.Unix(2, 0)},
			{Role: "assistant", Kind: "reasoning", Content: "middle reasoning that may be summarized", CreatedAt: time.Unix(3, 0)},
			{Role: "user", Kind: "request", Content: "keep final request", CreatedAt: time.Unix(4, 0)},
		},
	}

	compressed, report, err := compressor.CompressHistory(context.Background(), history)
	if err != nil {
		t.Fatalf("CompressHistory() error = %v, want nil", err)
	}

	for _, layer := range []int{1, 2, 3} {
		if !containsInt(report.LayersApplied, layer) {
			t.Errorf("CompressHistory() layers = %v, want layer %d", report.LayersApplied, layer)
		}
	}
	if report.Strategy != CompressionStrategyFourLayer {
		t.Errorf("CompressHistory() strategy = %v, want %v", report.Strategy, CompressionStrategyFourLayer)
	}
	if report.TokensAfter >= report.TokensBefore {
		t.Errorf("CompressHistory() token delta = %d -> %d, want reduction", report.TokensBefore, report.TokensAfter)
	}
	if report.Summary != "summary: keep the successful plan and final failure" {
		t.Errorf("CompressHistory() summary = %q, want model summary", report.Summary)
	}
	joined := renderMessages(compressed.Messages)
	for _, want := range []string{"keep root instruction", "summary: keep the successful plan", "keep final request"} {
		if !strings.Contains(joined, want) {
			t.Errorf("CompressHistory() messages missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, strings.Repeat("x", 40)) {
		t.Errorf("CompressHistory() kept untrimmed tool result:\n%s", joined)
	}
	if maas.callCount != 1 {
		t.Errorf("CompressHistory() summarizer calls = %d, want 1", maas.callCount)
	}
}

func TestContextCompressorFourLayerOnCJKHistoryWithDefaults(t *testing.T) {
	t.Parallel()

	maas := &recordingInferenceClient{response: "摘要：保留关键计划与最终结论"}
	compressor := NewContextCompressor(DefaultContextCompressorConfig(maas))
	// A long CJK middle message: each ideograph counts as one token under the
	// default CJK counter, so ~9600 chars far exceeds the 8000 TokenLimit and
	// forces the summarize layer. The whitespace counter would have scored this
	// whole block as a single "word" and never compressed.
	bigCJK := strings.Repeat("这是一段很长的中文推理内容需要被压缩以节省上下文", 400)
	history := MessageHistory{
		Messages: []Message{
			{Role: "system", Kind: "instruction", Content: "根指令必须保留", CreatedAt: time.Unix(1, 0)},
			{Role: "assistant", Kind: "reasoning", Content: "早期推理", CreatedAt: time.Unix(2, 0)},
			{Role: "assistant", Kind: "reasoning", Content: bigCJK, CreatedAt: time.Unix(3, 0)},
			{Role: "assistant", Kind: "reasoning", Content: "近期推理", CreatedAt: time.Unix(4, 0)},
			{Role: "user", Kind: "request", Content: "最终请求必须保留", CreatedAt: time.Unix(5, 0)},
		},
	}

	compressed, report, err := compressor.CompressHistory(context.Background(), history)
	if err != nil {
		t.Fatalf("CompressHistory() error = %v, want nil", err)
	}
	if report.Strategy != CompressionStrategyFourLayer {
		t.Fatalf("CompressHistory() strategy = %v, want FourLayer", report.Strategy)
	}
	if !containsInt(report.LayersApplied, 3) {
		t.Fatalf("CompressHistory() layers = %v, want summarize layer 3", report.LayersApplied)
	}
	if report.TokensAfter >= report.TokensBefore {
		t.Fatalf("CompressHistory() tokens %d -> %d, want reduction", report.TokensBefore, report.TokensAfter)
	}
	joined := renderMessages(compressed.Messages)
	for _, want := range []string{"根指令必须保留", "摘要：保留关键计划与最终结论", "最终请求必须保留"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("CompressHistory() missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, bigCJK) {
		t.Fatalf("CompressHistory() kept the uncompressed CJK block")
	}
}

func TestContextCompressorForceCheckpoint(t *testing.T) {
	t.Parallel()

	maas := &recordingInferenceClient{response: "checkpoint: task state is recoverable"}
	compressor := NewContextCompressor(ContextCompressorConfig{
		TokenLimit:    100,
		ProtectedHead: 1,
		ProtectedTail: 1,
		Summarizer:    maas,
	})
	history := MessageHistory{
		Messages: []Message{
			{Role: "system", Kind: "instruction", Content: "root"},
			{Role: "assistant", Kind: "reasoning", Content: "long running plan with enough detail to save"},
			{Role: "user", Kind: "request", Content: "continue"},
		},
	}

	checkpoint, err := compressor.ForceCheckpoint(context.Background(), 7, history)
	if err != nil {
		t.Fatalf("ForceCheckpoint() error = %v, want nil", err)
	}
	if checkpoint.CycleIndex != 7 {
		t.Errorf("ForceCheckpoint() cycle = %d, want 7", checkpoint.CycleIndex)
	}
	if checkpoint.Summary != "checkpoint: task state is recoverable" {
		t.Errorf("ForceCheckpoint() summary = %q, want model checkpoint", checkpoint.Summary)
	}
	if checkpoint.TokensSaved <= 0 {
		t.Errorf("ForceCheckpoint() tokens saved = %d, want positive", checkpoint.TokensSaved)
	}
	if maas.callCount != 1 {
		t.Errorf("ForceCheckpoint() summarizer calls = %d, want 1", maas.callCount)
	}
}

func TestContextCompressorNoLLMDegradesToTrimAndProtect(t *testing.T) {
	t.Parallel()

	compressor := NewContextCompressor(ContextCompressorConfig{
		TokenLimit:         12,
		ProtectedHead:      1,
		ProtectedTail:      1,
		ToolResultMaxChars: 8,
	})
	history := MessageHistory{
		Messages: []Message{
			{Role: "system", Kind: "instruction", Content: "root"},
			{Role: "tool", Kind: "tool_result", Content: "large tool " + strings.Repeat("z", 60)},
			{Role: "assistant", Kind: "reasoning", Content: "middle should be dropped without llm"},
			{Role: "user", Kind: "request", Content: "final"},
		},
	}

	compressed, report, err := compressor.CompressHistory(context.Background(), history)
	if err != nil {
		t.Fatalf("CompressHistory() error = %v, want nil", err)
	}
	if containsInt(report.LayersApplied, 3) {
		t.Errorf("CompressHistory() layers = %v, want no LLM summary layer", report.LayersApplied)
	}
	joined := renderMessages(compressed.Messages)
	for _, want := range []string{"root", "final"} {
		if !strings.Contains(joined, want) {
			t.Errorf("CompressHistory() messages missing protected content %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "middle should be dropped") {
		t.Errorf("CompressHistory() kept middle content without LLM:\n%s", joined)
	}
}

type recordingInferenceClient struct {
	response  string
	callCount int
}

func (c *recordingInferenceClient) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	c.callCount++
	return port.InferenceResponse{Text: c.response}, nil
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
