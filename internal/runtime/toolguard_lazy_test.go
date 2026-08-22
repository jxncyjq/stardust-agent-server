package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

// lazyGuardRegistry registers `count` distinct always-succeeding tools named
// tool_0..tool_N-1 so a lazy run can rotate through more of them than the
// per-tool loop cap allows for any single one.
func lazyGuardRegistry(count int) *tool.Registry {
	registry := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow), nil, tool.NoopGuardrails{})
	for i := 0; i < count; i++ {
		registry.RegisterDescriptor(
			tool.Descriptor{Name: fmt.Sprintf("tool_%d", i), Group: "probe"},
			tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
				return domain.ToolResult{CallID: call.ID, Success: true, Output: "ok"}, nil
			}),
		)
	}
	return registry
}

// rotatingCallToolMaas wraps every request in call_tool but rotates the real
// tool_name, so no single real tool ever approaches the cap. The guard must
// count the wrapped name, not the wrapper.
type rotatingCallToolMaas struct {
	calls    int
	distinct int
}

func (m *rotatingCallToolMaas) Generate(_ context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	m.calls++
	return port.InferenceResponse{
		Text: "working",
		ToolCalls: []domain.ToolCall{{
			ID:   fmt.Sprintf("c%d", m.calls),
			Name: metaToolCallTool,
			Arguments: map[string]string{
				"tool_name":      fmt.Sprintf("tool_%d", m.calls%m.distinct),
				"arguments_json": "{}",
			},
		}},
	}, nil
}

// A lazy run reaches every real tool through call_tool. Counting the wrapper
// name collapses every distinct tool onto one counter, turning the per-tool cap
// into a global one that cuts a healthy run short.
func TestLoopCapCountsWrappedToolNotCallTool(t *testing.T) {
	distinct := toolLoopCap + 5
	maas := &rotatingCallToolMaas{distinct: distinct}
	events := adapter.NewMemoryEventBus()

	rt := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:          maas,
		Tools:         lazyGuardRegistry(distinct),
		Events:        events,
		LazyTools:     true,
		MaxToolRounds: toolLoopCap + 10,
	})
	_, err := rt.RunTask(context.Background(), domain.Agent{ID: "a", Role: "developer"},
		domain.Task{ID: "t1", Status: domain.TaskRunning, Input: "go"})
	if err != nil {
		t.Fatalf("RunTask err = %v", err)
	}

	published, err := events.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	// Every real tool is used at most twice here, far under the cap. A cap event
	// can only mean the wrapper name was counted, collapsing all `distinct`
	// tools onto one counter.
	for _, ev := range published {
		if ev.Type == "tool_loop_broken" {
			t.Fatalf("healthy run cut by the loop cap: %q — %d distinct wrapped tools "+
				"shared one counter", ev.Message, distinct)
		}
	}
}

// sameWrappedToolMaas hammers ONE real tool through call_tool with different
// arguments each round, so only the per-tool-name cap can stop it.
type sameWrappedToolMaas struct{ calls int }

func (m *sameWrappedToolMaas) Generate(_ context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	m.calls++
	return port.InferenceResponse{
		Text: "working",
		ToolCalls: []domain.ToolCall{{
			ID:   fmt.Sprintf("c%d", m.calls),
			Name: metaToolCallTool,
			Arguments: map[string]string{
				"tool_name":      "tool_0",
				"arguments_json": fmt.Sprintf(`{"n":"%d"}`, m.calls),
			},
		}},
	}, nil
}

func TestLoopCapStopsWrappedRunawayAndNamesTheRealTool(t *testing.T) {
	maas := &sameWrappedToolMaas{}
	events := adapter.NewMemoryEventBus()

	rt := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:          maas,
		Tools:         lazyGuardRegistry(1),
		Events:        events,
		LazyTools:     true,
		MaxToolRounds: toolLoopCap + 20,
	})
	_, err := rt.RunTask(context.Background(), domain.Agent{ID: "a", Role: "developer"},
		domain.Task{ID: "t1", Status: domain.TaskRunning, Input: "go"})
	if err != nil {
		t.Fatalf("RunTask err = %v", err)
	}
	if maas.calls > toolLoopCap+3 {
		t.Fatalf("model called %d times; loop cap (%d) should have cut it", maas.calls, toolLoopCap)
	}

	published, err := events.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var broken string
	for _, ev := range published {
		if ev.Type == "tool_loop_broken" {
			broken = ev.Message
		}
	}
	if broken == "" {
		t.Fatal("no tool_loop_broken event published")
	}
	if !strings.Contains(broken, "tool_0") {
		t.Fatalf("event must name the real runaway tool, got %q", broken)
	}
	if strings.Contains(broken, metaToolCallTool) {
		t.Fatalf("event must not blame the call_tool wrapper, got %q", broken)
	}
}

// failingWrappedToolMaas drives one real tool that always fails, through
// call_tool, so the same-tool-failure warning must name the real tool.
type failingWrappedToolMaas struct {
	calls    int
	requests []port.InferenceRequest
}

func (m *failingWrappedToolMaas) Generate(_ context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	m.calls++
	m.requests = append(m.requests, req)
	if m.calls > toolSameFailWarn+2 {
		return port.InferenceResponse{Text: "giving up"}, nil
	}
	return port.InferenceResponse{
		Text: "retrying",
		ToolCalls: []domain.ToolCall{{
			ID:   fmt.Sprintf("c%d", m.calls),
			Name: metaToolCallTool,
			Arguments: map[string]string{
				"tool_name":      "flaky",
				"arguments_json": fmt.Sprintf(`{"n":"%d"}`, m.calls),
			},
		}},
	}, nil
}

func TestSameToolFailureWarningNamesWrappedTool(t *testing.T) {
	registry := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow), nil, tool.NoopGuardrails{})
	registry.RegisterDescriptor(
		tool.Descriptor{Name: "flaky", Group: "probe"},
		tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{CallID: call.ID, Success: false, Error: "boom"}, nil
		}),
	)
	maas := &failingWrappedToolMaas{}
	rt := NewRuntime(Config{Gate: NewTaskGate(), Maas: maas, Tools: registry, LazyTools: true, MaxToolRounds: 50})
	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a", Role: "developer"},
		domain.Task{ID: "t1", Status: domain.TaskRunning, Input: "go"}); err != nil {
		t.Fatalf("RunTask err = %v", err)
	}

	warning := ""
	for _, req := range maas.requests {
		for _, msg := range req.Messages {
			if strings.Contains(msg.Content, "已累计失败") {
				warning = msg.Content
			}
		}
	}
	if warning == "" {
		t.Fatal("no same-tool-failure warning was shown to the model")
	}
	if !strings.Contains(warning, "flaky") {
		t.Fatalf("warning must name the real failing tool, got %q", warning)
	}
	if strings.Contains(warning, metaToolCallTool) {
		t.Fatalf("warning must not blame the call_tool wrapper, got %q", warning)
	}
}
