package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

// recordingRoundsMaas replays scripted responses and keeps every request, so a
// test can assert on what the model was shown in a given round.
type recordingRoundsMaas struct {
	mu        sync.Mutex
	responses []port.InferenceResponse
	requests  []port.InferenceRequest
}

func (m *recordingRoundsMaas) Generate(_ context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := req.Validate(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.requests = append(m.requests, req)
	if len(m.requests) <= len(m.responses) {
		return m.responses[len(m.requests)-1], nil
	}
	return port.InferenceResponse{Text: "done"}, nil
}

// loopingMaas always asks for the same tool call with the same arguments — the
// exact behaviour observed on 2026-07-23, when one task read hello.txt 152
// times. It answers in text only once no tools are offered.
type loopingMaas struct {
	mu       sync.Mutex
	call     domain.ToolCall
	calls    int
	requests []port.InferenceRequest
}

func (m *loopingMaas) Generate(_ context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := req.Validate(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.calls++
	m.requests = append(m.requests, req)
	if len(req.Tools) == 0 {
		return port.InferenceResponse{Text: "final answer without tools"}, nil
	}
	call := m.call
	call.ID = "call-" + strings.Repeat("x", m.calls)
	return port.InferenceResponse{ToolCalls: []domain.ToolCall{call}}, nil
}

func (m *loopingMaas) sawText(substr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, req := range m.requests {
		for _, msg := range req.Messages {
			if strings.Contains(msg.Content, substr) {
				return true
			}
		}
	}
	return false
}

// unchangingReadRegistry offers one read_file that always returns the same
// content, so the loop is driven purely by what the model asks for — never by a
// tool failure or by the file changing under it.
func unchangingReadRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"read_file"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "read_file",
		Description: "read a file",
		InputSchema: map[string]any{"properties": map[string]any{"path": map[string]any{"type": "string"}}},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: call.ID, Success: true, Output: "unchanged content"}, nil
	}))
	return registry
}

// The regression this whole change exists for: from the second round on, the
// model must receive its own previous tool call and that call's result as
// separate turns.
func TestSecondRoundCarriesPriorAssistantCallAndToolResult(t *testing.T) {
	t.Parallel()
	maas := &recordingRoundsMaas{responses: []port.InferenceResponse{
		{ToolCalls: []domain.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "hello.txt"}}}},
		{Text: "done"},
	}}
	rt := NewRuntime(Config{Maas: maas, Tools: unchangingReadRegistry(t)})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read hello.txt"}); err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}

	if len(maas.requests) < 2 {
		t.Fatalf("model was called %d times, want at least 2", len(maas.requests))
	}
	second := maas.requests[1]
	if second.Prompt != "" {
		t.Fatalf("second request Prompt = %q, want empty (multi-turn requests carry Messages)", second.Prompt)
	}
	var sawAssistantCall, sawToolResult bool
	for _, msg := range second.Messages {
		if msg.Role == port.RoleAssistant && len(msg.ToolCalls) == 1 && msg.ToolCalls[0].Name == "read_file" {
			sawAssistantCall = true
		}
		if msg.Role == port.RoleTool && msg.ToolCallID == "c1" && msg.Content == "unchanged content" {
			sawToolResult = true
		}
	}
	if !sawAssistantCall || !sawToolResult {
		t.Fatalf("second request messages = %+v, want the model's own call and its result", second.Messages)
	}
}

// With max_tool_rounds unlimited, a model stuck on one call must still be
// stopped — and warned before it is.
func TestRuntimeBreaksRepeatedIdenticalToolCallLoop(t *testing.T) {
	t.Parallel()
	maas := &loopingMaas{call: domain.ToolCall{Name: "read_file", Arguments: map[string]string{"path": "hello.txt"}}}
	rt := NewRuntime(Config{Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: 0})

	run, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read it"})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}

	if maas.calls > repeatAbortStreak+2 {
		t.Fatalf("model was called %d times, want the loop cut near the abort streak (%d)", maas.calls, repeatAbortStreak)
	}
	if run.Result == "" {
		t.Fatal("run.Result is empty: a cut loop must still answer the user")
	}
	if !maas.sawText("连续") {
		t.Fatal("the model was never told it was repeating itself before the loop was cut")
	}
}

// A model doing different work each round must not be interrupted.
func TestRuntimeDoesNotBreakLoopOnDistinctCalls(t *testing.T) {
	t.Parallel()
	responses := make([]port.InferenceResponse, 0, repeatAbortStreak+1)
	for i := range repeatAbortStreak + 1 {
		args, err := json.Marshal(i)
		if err != nil {
			t.Fatalf("Marshal(round index) error = %v, want nil", err)
		}
		responses = append(responses, port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "c" + string(args),
			Name:      "read_file",
			Arguments: map[string]string{"path": "file-" + string(args) + ".txt"},
		}}})
	}
	responses = append(responses, port.InferenceResponse{Text: "done"})
	maas := &recordingRoundsMaas{responses: responses}
	rt := NewRuntime(Config{Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: len(responses) + 2})

	run, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read many files"})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}

	if run.Result != "done" {
		t.Fatalf("run.Result = %q, want the model's own answer after distinct rounds", run.Result)
	}
	if len(maas.requests) != len(responses) {
		t.Fatalf("model was called %d times, want %d (no round was cut)", len(maas.requests), len(responses))
	}
}
