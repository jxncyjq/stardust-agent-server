package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/tool"
)

// scriptedMaas returns a tool call on its first inference and a final text answer
// afterwards, so a single tool round is exercised.
type scriptedMaas struct {
	mu    sync.Mutex
	calls int
}

func (m *scriptedMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls == 1 {
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "call-1",
			Name:      "echo",
			Arguments: map[string]string{"text": "hi"},
		}}}, nil
	}
	return port.InferenceResponse{Text: "final answer"}, nil
}

// gateOnce suspends exactly once, then allows — modelling "decision arrived".
type gateOnce struct {
	mu        sync.Mutex
	suspended bool
}

func (g *gateOnce) ShouldSuspend(_ context.Context, _ domain.Task, _ []domain.ToolCall, _ *tool.Registry) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.suspended {
		g.suspended = true
		return true, nil
	}
	return false, nil
}

func (g *gateOnce) Resolve(context.Context, domain.Task, domain.ToolCall, *tool.Registry) (bool, error) {
	return true, nil
}

// echoRegistry returns a registry with a single auto-allowed "echo" tool, so
// executeToolCalls has something real to run on resume.
func echoRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"echo"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	)
	reg.RegisterDescriptor(tool.Descriptor{
		Name:        "echo",
		Description: "echo text back",
		InputSchema: map[string]any{"properties": map[string]any{"text": map[string]any{"type": "string"}}},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: call.ID, Success: true, Output: call.Arguments["text"]}, nil
	}))
	return reg
}

func TestRunTaskSuspendsAndWritesCheckpoint(t *testing.T) {
	store := sessionstate.NewStore(t.TempDir())
	runner := NewRuntime(Config{
		Maas:        &scriptedMaas{},
		Audit:       adapter.NewMemoryAuditLog(),
		Events:      adapter.NewMemoryEventBus(),
		Tools:       echoRegistry(t),
		Checkpoints: store,
		ToolGate:    &gateOnce{},
	})
	task := domain.Task{ID: "task-1", SessionID: "sess-1", AgentID: "agent-1", Status: domain.TaskRunning, Input: "go"}

	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, task)
	if !errors.Is(err, ErrSuspended) {
		t.Fatalf("RunTask err = %v, want ErrSuspended", err)
	}
	cp, ok, err := store.Load("sess-1")
	if err != nil {
		t.Fatalf("Load checkpoint: %v", err)
	}
	if !ok {
		t.Fatal("no checkpoint written on suspend")
	}
	if len(cp.PendingCalls) != 1 || cp.PendingCalls[0].Name != "echo" {
		t.Errorf("checkpoint PendingCalls = %#v, want one echo call", cp.PendingCalls)
	}
}

func TestRunTaskResumesFromCheckpointToCompletion(t *testing.T) {
	store := sessionstate.NewStore(t.TempDir())
	maas := &scriptedMaas{}
	gate := &gateOnce{}
	cfg := Config{
		Maas:        maas,
		Audit:       adapter.NewMemoryAuditLog(),
		Events:      adapter.NewMemoryEventBus(),
		Tools:       echoRegistry(t),
		Checkpoints: store,
		ToolGate:    gate,
	}
	runner := NewRuntime(cfg)
	task := domain.Task{ID: "task-1", SessionID: "sess-1", AgentID: "agent-1", Status: domain.TaskRunning, Input: "go"}

	// First run suspends.
	if _, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, task); !errors.Is(err, ErrSuspended) {
		t.Fatalf("first run err = %v, want ErrSuspended", err)
	}
	// Second run (same runtime, gate now allows) auto-resumes from the checkpoint.
	run, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, task)
	if err != nil {
		t.Fatalf("resume run err = %v, want nil", err)
	}
	if run.Result != "final answer" {
		t.Errorf("resume result = %q, want %q", run.Result, "final answer")
	}
	// Checkpoint deleted after successful completion.
	_, ok, err := store.Load("sess-1")
	if err != nil {
		t.Fatalf("Load after completion: %v", err)
	}
	if ok {
		t.Error("checkpoint still present after successful completion, want deleted")
	}
}
