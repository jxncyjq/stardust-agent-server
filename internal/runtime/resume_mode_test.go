package runtime

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/taskgate"
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
	// toolArgs are the arguments sent with the round-1 call. Nil defaults to an
	// empty map; tests exercising a tool named "write_file" must set a "path"
	// entry, since the runtime's tool loop treats a pathless write_file success
	// as an invariant violation (see runtime.go executeToolCalls).
	toolArgs map[string]string
	// usage 是第 1 轮响应报告的 token 用量。零值（默认）表示不报，与既有用例一致；
	// 需要让 checkpoint 里存下**非零的累计用量**的用例（恢复路径的 usage 语义）
	// 必须设它，否则那条断言在全零的日志上永远成立、等于没测。
	usage port.InferenceResponse
	calls int
}

func (m *oneToolThenTextMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.calls++
	if m.calls == 1 {
		args := m.toolArgs
		if args == nil {
			args = map[string]string{}
		}
		resp := m.usage
		resp.ToolCalls = []domain.ToolCall{{ID: "c1", Name: m.toolName, Arguments: args}}
		return resp, nil
	}
	return port.InferenceResponse{Text: "done"}, nil
}

func TestCheckSuspendPersistsMode(t *testing.T) {
	dir := t.TempDir()
	store := sessionstate.NewStore(dir)
	reg := planRegistry(t) // reuses helper from plan_mode_test.go (read_x safe, write_x sensitive)
	maas := &oneToolThenTextMaas{toolName: "read_x"}
	runner := NewRuntime(Config{Gate: taskgate.NewTaskGate(),
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
