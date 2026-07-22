package runtime

import (
	"context"
	"errors"
	"strings"
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
	cp, ok, err := store.Load("sess-1", "")
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
	_, ok, err := store.Load("sess-1", "")
	if err != nil {
		t.Fatalf("Load after completion: %v", err)
	}
	if ok {
		t.Error("checkpoint still present after successful completion, want deleted")
	}
}

// scriptedLoadThenToolMaas drives a run that first pins a capability's
// definition via load_capabilities, then issues a real tool call -- so the
// run suspends (via gateSuspendOnSecondShouldSuspend below) with a non-empty
// loaded block already accumulated but the real tool call still pending.
type scriptedLoadThenToolMaas struct {
	mu    sync.Mutex
	calls int
}

func (m *scriptedLoadThenToolMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls == 1 {
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID: "call-load", Name: metaToolLoadCapabilities, Arguments: map[string]string{"names": "echo"},
		}}}, nil
	}
	return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
		ID: "call-echo", Name: "echo", Arguments: map[string]string{"text": "hi"},
	}}}, nil
}

// gateSuspendOnSecondShouldSuspend allows the first tool round through (so
// load_capabilities actually executes and populates the loaded block) and
// suspends on the second (before the real "echo" call executes), so the
// checkpoint captures a non-empty loaded block alongside a still-pending call.
type gateSuspendOnSecondShouldSuspend struct {
	mu    sync.Mutex
	calls int
}

func (g *gateSuspendOnSecondShouldSuspend) ShouldSuspend(_ context.Context, _ domain.Task, _ []domain.ToolCall, _ *tool.Registry) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.calls == 2, nil
}

func (g *gateSuspendOnSecondShouldSuspend) Resolve(context.Context, domain.Task, domain.ToolCall, *tool.Registry) (bool, error) {
	return true, nil
}

// lookupRegistryWithGroup returns a registry with a single "echo" tool whose
// Group is set (required by capability.ToolProvider) and whose Description
// carries a marker distinctive enough to grep for in a rendered prompt.
func lookupRegistryWithGroup() *tool.Registry {
	reg := tool.NewRegistry(nil, nil, nil)
	reg.RegisterDescriptor(tool.Descriptor{
		Name:        "echo",
		Group:       "files",
		Description: "ECHO-SCHEMA-MARKER",
		InputSchema: map[string]any{"properties": map[string]any{"text": map[string]any{"type": "string"}}},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: call.ID, Success: true, Output: call.Arguments["text"]}, nil
	}))
	return reg
}

// TestLoadedCapabilitiesPersistAcrossSuspendResume pins the Task 8 guarantee:
// a capability the model pinned into the loaded block before suspending is
// still there after a resume (even across a simulated process restart), so
// the resumed run does not have to call load_capabilities again for it.
//
// The mutation this guards against is exactly the one called out in the Task
// 8 brief: restoreLoaded's result silently discarded (not assigned to
// loopState.loaded) on the resume path. If that happened, the resumed run's
// composePrompt would render no "Loaded capabilities:" section at all, and
// this test's final assertion would fail.
func TestLoadedCapabilitiesPersistAcrossSuspendResume(t *testing.T) {
	dir := t.TempDir()
	agent := domain.Agent{ID: "agent-1"}
	task := domain.Task{ID: "task-loaded", SessionID: "sess-loaded", AgentID: "agent-1", Status: domain.TaskRunning, Input: "go"}
	ctx := context.Background()

	// --- process 1: load a capability, then suspend before the pending real call ---
	store1 := sessionstate.NewStore(dir)
	runner1 := NewRuntime(Config{
		Maas:        &scriptedLoadThenToolMaas{},
		Audit:       adapter.NewMemoryAuditLog(),
		Events:      adapter.NewMemoryEventBus(),
		Tools:       lookupRegistryWithGroup(),
		Checkpoints: store1,
		ToolGate:    &gateSuspendOnSecondShouldSuspend{},
		LazyTools:   true,
	})
	if _, err := runner1.RunTask(ctx, agent, task); !errors.Is(err, ErrSuspended) {
		t.Fatalf("process1 run err = %v, want ErrSuspended", err)
	}

	cp, ok, err := store1.Load("sess-loaded", "")
	if err != nil || !ok {
		t.Fatalf("Load checkpoint: ok=%v err=%v", ok, err)
	}
	if len(cp.Loaded) != 1 || cp.Loaded[0].Name != "echo" {
		t.Fatalf("checkpoint Loaded = %#v, want one echo entry", cp.Loaded)
	}
	if !strings.Contains(cp.Loaded[0].Detail, "ECHO-SCHEMA-MARKER") {
		t.Fatalf("checkpoint Loaded[0].Detail = %q, want it to carry the tool's schema", cp.Loaded[0].Detail)
	}
	if len(cp.PendingCalls) != 1 || cp.PendingCalls[0].Name != "echo" {
		t.Fatalf("checkpoint PendingCalls = %#v, want the still-pending echo call", cp.PendingCalls)
	}

	// --- simulate restart: brand-new store + runtime over the same dir ---
	store2 := sessionstate.NewStore(dir)
	resumeMaas := &recordingSubMaas{summary: "final answer"}
	runner2 := NewRuntime(Config{
		Maas:        resumeMaas,
		Audit:       adapter.NewMemoryAuditLog(),
		Events:      adapter.NewMemoryEventBus(),
		Tools:       lookupRegistryWithGroup(),
		Checkpoints: store2,
		ToolGate:    nil,
		LazyTools:   true,
	})
	run, err := runner2.RunTask(ctx, agent, task)
	if err != nil {
		t.Fatalf("resume run err = %v, want nil", err)
	}
	if run.Result != "final answer" {
		t.Fatalf("resume result = %q, want %q", run.Result, "final answer")
	}
	prompts := resumeMaas.recorded()
	if len(prompts) != 1 {
		t.Fatalf("resume prompts = %d, want 1 (the round after the restored pending echo call executes)", len(prompts))
	}
	if !strings.Contains(prompts[0], "Loaded capabilities:") {
		t.Fatalf("resumed prompt missing the loaded-capabilities block entirely:\n%s", prompts[0])
	}
	if !strings.Contains(prompts[0], "ECHO-SCHEMA-MARKER") {
		t.Fatalf("resumed prompt does not carry the pre-suspend loaded capability's detail: restoreLoaded's result was not applied to the resumed loop state\n%s", prompts[0])
	}
}
