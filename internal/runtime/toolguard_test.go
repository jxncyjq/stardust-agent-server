package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/testsupport"
	"github.com/stardust/legion-agent/internal/tool"
)

// alwaysFetchDifferentArgsMaas asks for fetch_url every round with a DIFFERENT
// url each time, so the callsKey(name+args) signature guard never trips — only
// the per-tool-name loop cap can stop it.
type alwaysFetchDifferentArgsMaas struct{ calls int }

func (m *alwaysFetchDifferentArgsMaas) Generate(_ context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	m.calls++
	return port.InferenceResponse{
		Text: "fetching",
		ToolCalls: []domain.ToolCall{{
			ID:        fmt.Sprintf("c%d", m.calls),
			Name:      "fetch_url",
			Arguments: map[string]string{"url": fmt.Sprintf("https://ex.example/%d", m.calls)},
		}},
	}, nil
}

func TestLoopCapStopsSameToolDifferentArgs(t *testing.T) {
	maas := &alwaysFetchDifferentArgsMaas{}
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow), nil, tool.NoopGuardrails{})
	reg.RegisterDescriptor(
		tool.Descriptor{Name: "fetch_url", Group: "web"},
		tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{CallID: call.ID, Success: true, Output: "ok"}, nil
		}),
	)
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: reg, MaxToolRounds: 1000})
	_, err := rt.RunTask(context.Background(), domain.Agent{ID: "a", Role: "developer"},
		domain.Task{ID: "t1", Status: domain.TaskRunning, Input: "go"})
	if err != nil {
		t.Fatalf("RunTask err = %v", err)
	}
	if maas.calls > toolLoopCap+3 {
		t.Fatalf("model called %d times; loop cap (%d) should have cut it", maas.calls, toolLoopCap)
	}
	if maas.calls < toolLoopCap {
		t.Fatalf("model called only %d times; expected to reach loop cap (%d) first", maas.calls, toolLoopCap)
	}
}

// recordingFailMaas asks for fetch_url every round (different args each time,
// so the callsKey signature guard never trips) and records every request it
// receives, so the test can inspect what the model was shown after each
// round. It stops asking for tools once it has been called enough times to
// have observed the same-tool-failure warning, ending the task.
type recordingFailMaas struct {
	calls    int
	requests []port.InferenceRequest
}

func (m *recordingFailMaas) Generate(_ context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	m.requests = append(m.requests, req)
	m.calls++
	if m.calls > toolSameFailWarn+2 {
		return port.InferenceResponse{Text: "giving up"}, nil
	}
	return port.InferenceResponse{
		Text: "retry",
		ToolCalls: []domain.ToolCall{{
			ID:        fmt.Sprintf("c%d", m.calls),
			Name:      "fetch_url",
			Arguments: map[string]string{"url": fmt.Sprintf("https://ex.example/%d", m.calls)},
		}},
	}, nil
}

// TestSameToolFailureWarnsModel exercises the P2 same-tool-failure warning:
// when one tool (by name) fails toolSameFailWarn times, the runtime must
// inject a system turn telling the model to stop retrying it. The loop cap
// (toolLoopCap) is a much higher hard stop; this warning is meant to fire
// long before that, on repeated FAILURES specifically.
func TestSameToolFailureWarnsModel(t *testing.T) {
	maas := &recordingFailMaas{}
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow), nil, tool.NoopGuardrails{})
	reg.RegisterDescriptor(
		tool.Descriptor{Name: "fetch_url", Group: "web"},
		tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{CallID: call.ID, Success: false, Error: "boom"}, nil
		}),
	)
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: reg, MaxToolRounds: 1000})
	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a", Role: "developer"},
		domain.Task{ID: "t1", Status: domain.TaskRunning, Input: "go"}); err != nil {
		t.Fatalf("RunTask err = %v", err)
	}
	warned := false
	for _, req := range maas.requests {
		text := testsupport.RequestText(req)
		if strings.Contains(text, "工具 fetch_url") && strings.Contains(text, "失败") {
			warned = true
			break
		}
	}
	if !warned {
		t.Fatalf("expected a same-tool-failure warning after %d failures, none found", toolSameFailWarn)
	}
}
