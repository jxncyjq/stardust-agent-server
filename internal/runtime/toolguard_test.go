package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
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
	rt := NewRuntime(Config{Maas: maas, Tools: reg, MaxToolRounds: 1000})
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
