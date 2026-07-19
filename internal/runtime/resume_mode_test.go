package runtime

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/tool"
)

// alwaysSuspendGate suspends on the first round and, once a checkpoint exists,
// resolves every call as allowed — the minimal gate to exercise mode plumbing.
type alwaysSuspendGate struct{ suspended bool }

func (g *alwaysSuspendGate) ShouldSuspend(_ context.Context, _ domain.Task, calls []domain.ToolCall, _ *tool.Registry) (bool, error) {
	if g.suspended {
		return false, nil
	}
	g.suspended = true
	return len(calls) > 0, nil
}
func (g *alwaysSuspendGate) Resolve(context.Context, domain.Task, domain.ToolCall, *tool.Registry) (bool, error) {
	return true, nil
}

// oneToolThenTextMaas issues a single tool call on the first round, then answers
// in text on every subsequent round.
type oneToolThenTextMaas struct {
	toolName string
	calls    int
}

func (m *oneToolThenTextMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.calls++
	if m.calls == 1 {
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{ID: "c1", Name: m.toolName, Arguments: map[string]string{}}}}, nil
	}
	return port.InferenceResponse{Text: "done"}, nil
}

func TestCheckSuspendPersistsMode(t *testing.T) {
	dir := t.TempDir()
	store := sessionstate.NewStore(dir)
	reg := planRegistry(t) // reuses helper from plan_mode_test.go (read_x safe, write_x sensitive)
	maas := &oneToolThenTextMaas{toolName: "read_x"}
	runner := NewRuntime(Config{
		Maas: maas, Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: reg, Checkpoints: store, ToolGate: &alwaysSuspendGate{},
	})
	task := domain.Task{ID: "t1", SessionID: "s1", AgentID: "a1", Status: domain.TaskRunning, Mode: domain.ModeManual, Input: "go"}
	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "a1"}, task)
	if err != ErrSuspended {
		t.Fatalf("RunTask err = %v, want ErrSuspended", err)
	}
	cp, ok, err := store.Load("s1", "")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if cp.Mode != domain.ModeManual {
		t.Fatalf("checkpoint Mode = %q, want manual", cp.Mode)
	}
}
