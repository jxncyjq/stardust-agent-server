package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/testsupport"
	"github.com/stardust/legion-agent/internal/tool"
)

func TestRuntimeRunTaskCompletesThroughMaasPort(t *testing.T) {
	t.Parallel()

	maas := adapter.NewRecordingMaas("done")
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:   maas,
		Audit:  audit,
		Events: events,
	})

	run, err := runner.RunTask(context.Background(), domain.Agent{
		ID:        "agent-1",
		CompanyID: "company-1",
		Role:      "developer",
		Status:    domain.AgentActive,
	}, domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Status:    domain.TaskRunning,
		Input:     "say done",
	})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if run.Result != "done" {
		t.Errorf("RunTask() result = %q, want %q", run.Result, "done")
	}
	if maas.CallCount() != 1 {
		t.Errorf("MaasInferenceClient calls = %d, want 1", maas.CallCount())
	}
	auditEvents := mustAuditEvents(t, audit)
	if len(auditEvents) == 0 {
		t.Errorf("AuditLog events = 0, want at least 1")
	}
	runtimeEvents := mustRuntimeEvents(t, events)
	if !hasLearningRuntimeEvent(runtimeEvents, evolution.SignalSuccess) {
		t.Errorf("Runtime events missing learning success event: %#v", runtimeEvents)
	}
}

func TestRuntimeRunTaskPublishesLightweightFailureLearningEvent(t *testing.T) {
	t.Parallel()

	events := adapter.NewMemoryEventBus()
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:   failingMaas{},
		Audit:  adapter.NewMemoryAuditLog(),
		Events: events,
	})

	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, domain.Task{
		ID:      "task-fail",
		AgentID: "agent-1",
		Input:   "fail inference",
	})
	if err == nil {
		t.Fatalf("RunTask() error = nil, want error")
	}
	runtimeEvents := mustRuntimeEvents(t, events)
	if !hasLearningRuntimeEvent(runtimeEvents, evolution.SignalFailure) {
		t.Fatalf("Runtime events missing learning failure event: %#v", runtimeEvents)
	}
	if !hasLearningMessagePart(runtimeEvents, "lightweight=true") {
		t.Fatalf("Runtime learning failure event missing lightweight=true: %#v", runtimeEvents)
	}
}

// fakeEpisodeRecorder is a test double for EpisodeRecorder: it records every
// call it receives (outcome, content, task ID) under a mutex so tests can
// assert on them after RunTask returns.
type fakeEpisodeRecorder struct {
	mu    sync.Mutex
	calls []struct{ outcome, content, taskID string }
}

func (f *fakeEpisodeRecorder) RecordEpisode(_ domain.Agent, task domain.Task, outcome, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct{ outcome, content, taskID string }{outcome, content, task.ID})
}

func TestRuntimeRecordsSuccessEpisode(t *testing.T) {
	t.Parallel()

	rec := &fakeEpisodeRecorder{}
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:            adapter.NewRecordingMaas("done"),
		Audit:           adapter.NewMemoryAuditLog(),
		Events:          adapter.NewMemoryEventBus(),
		EpisodeRecorder: rec,
	})

	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, domain.Task{
		ID:      "task-episode-success",
		AgentID: "agent-1",
		Input:   "say done",
	})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 {
		t.Fatalf("EpisodeRecorder calls = %d, want 1: %+v", len(rec.calls), rec.calls)
	}
	if rec.calls[0].outcome != "success" {
		t.Fatalf("EpisodeRecorder call outcome = %q, want %q", rec.calls[0].outcome, "success")
	}
	if rec.calls[0].taskID != "task-episode-success" {
		t.Fatalf("EpisodeRecorder call taskID = %q, want %q", rec.calls[0].taskID, "task-episode-success")
	}
	if rec.calls[0].content != "done" {
		t.Fatalf("EpisodeRecorder call content = %q, want the task's result text %q", rec.calls[0].content, "done")
	}
}

func TestRuntimeRecordsFailureEpisode(t *testing.T) {
	t.Parallel()

	rec := &fakeEpisodeRecorder{}
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:            failingMaas{},
		Audit:           adapter.NewMemoryAuditLog(),
		Events:          adapter.NewMemoryEventBus(),
		EpisodeRecorder: rec,
	})

	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, domain.Task{
		ID:      "task-episode-failure",
		AgentID: "agent-1",
		Input:   "fail inference",
	})
	if err == nil {
		t.Fatalf("RunTask() error = nil, want error")
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 {
		t.Fatalf("EpisodeRecorder calls = %d, want 1: %+v", len(rec.calls), rec.calls)
	}
	if !strings.HasPrefix(rec.calls[0].outcome, "failure:") {
		t.Fatalf("EpisodeRecorder call outcome = %q, want failure:<reason> prefix", rec.calls[0].outcome)
	}
	if rec.calls[0].taskID != "task-episode-failure" {
		t.Fatalf("EpisodeRecorder call taskID = %q, want %q", rec.calls[0].taskID, "task-episode-failure")
	}
	if rec.calls[0].content != "fail inference" {
		t.Fatalf("EpisodeRecorder call content = %q, want the task's input %q", rec.calls[0].content, "fail inference")
	}
}

func TestRuntimeUsesNoopPortsWhenAuditAndEventsMissing(t *testing.T) {
	t.Parallel()

	maas := &captureMaas{response: "done without optional ports", reasoning: "reasoned through noop ports"}
	runner := NewRuntime(Config{Gate: NewTaskGate(), Maas: maas})
	run, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, domain.Task{
		ID:    "task-noop-ports",
		Input: "run without optional ports",
	})
	if err != nil {
		t.Fatalf("RunTask(noop ports) error = %v, want nil", err)
	}
	if run.Result != "done without optional ports" {
		t.Fatalf("RunTask(noop ports).Result = %q, want done without optional ports", run.Result)
	}
	if run.ReasoningSummary != "reasoned through noop ports" {
		t.Fatalf("RunTask(noop ports).ReasoningSummary = %q, want reasoning", run.ReasoningSummary)
	}
	if maas.prompt != "run without optional ports" {
		t.Fatalf("RunTask(noop ports) MaaS prompt = %q, want task input", maas.prompt)
	}
}

func TestRuntimeRecordsTokenUsageOnModelInferenceCompletedAudit(t *testing.T) {
	t.Parallel()

	maas := &captureMaas{
		response:         "done",
		promptTokens:     11,
		completionTokens: 22,
		cachedTokens:     3,
		totalTokens:      33,
	}
	audit := adapter.NewMemoryAuditLog()
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:   maas,
		Audit:  audit,
		Events: adapter.NewMemoryEventBus(),
	})

	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, domain.Task{
		ID:    "task-token-audit",
		Input: "do the task",
	})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}

	auditEvents := mustAuditEvents(t, audit)
	event, ok := findAuditEvent(auditEvents, "model_inference_completed")
	if !ok {
		t.Fatalf("audit events missing model_inference_completed: %#v", auditEvents)
	}
	if event.PromptTokens != 11 || event.CompletionTokens != 22 || event.CachedTokens != 3 || event.TotalTokens != 33 {
		t.Fatalf("model_inference_completed audit tokens = %d/%d/%d/%d, want 11/22/3/33",
			event.PromptTokens, event.CompletionTokens, event.CachedTokens, event.TotalTokens)
	}
}

func TestRuntimeMissingMaasReturnsErrMaasUnavailable(t *testing.T) {
	t.Parallel()

	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
	})
	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, domain.Task{
		ID:    "task-no-maas",
		Input: "cannot infer",
	})
	if !errors.Is(err, ErrMaasUnavailable) {
		t.Fatalf("RunTask(missing maas) error = %v, want ErrMaasUnavailable", err)
	}
}

func TestRuntimeRunTaskIncludesContextPrefixInPrompt(t *testing.T) {
	t.Parallel()

	maas := &captureMaas{response: "done"}
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:          maas,
		Audit:         adapter.NewMemoryAuditLog(),
		Events:        adapter.NewMemoryEventBus(),
		ContextPrefix: "Agent identity:\nLegion Soul",
	})
	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, domain.Task{
		ID:    "task-context",
		Input: "do the task",
	})
	if err != nil {
		t.Fatalf("RunTask(context prefix) error = %v, want nil", err)
	}
	if !strings.Contains(maas.prompt, "Legion Soul") {
		t.Fatalf("RunTask(context prefix) prompt = %q, want context prefix", maas.prompt)
	}
	if !strings.Contains(maas.prompt, "do the task") {
		t.Fatalf("RunTask(context prefix) prompt = %q, want task input", maas.prompt)
	}
}

func TestRuntimeRunTaskBuildsPromptWithCognitiveCore(t *testing.T) {
	t.Parallel()

	maas := &captureMaas{response: "done"}
	core := cognitive.NewCore(cognitive.NoopCompressor{}).WithContextFiles("Agent identity:\nLegion Soul")
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:           maas,
		Audit:          adapter.NewMemoryAuditLog(),
		Events:         adapter.NewMemoryEventBus(),
		ContextBuilder: core,
	})
	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.Task{
		ID:    "task-cognitive",
		Input: "do the cognitive task",
	})
	if err != nil {
		t.Fatalf("RunTask(cognitive core) error = %v, want nil", err)
	}
	for _, want := range []string{"Runtime context files:", "Legion Soul", "Task: task-cognitive", "do the cognitive task"} {
		if !strings.Contains(maas.prompt, want) {
			t.Fatalf("RunTask(cognitive core) prompt missing %q:\n%s", want, maas.prompt)
		}
	}
}

func TestRuntimeRunTaskPassesConversationTurnsToCognitiveCore(t *testing.T) {
	t.Parallel()

	maas := &captureMaas{response: "done"}
	core := cognitive.NewCore(cognitive.NoopCompressor{})
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:              maas,
		Audit:             adapter.NewMemoryAuditLog(),
		Events:            adapter.NewMemoryEventBus(),
		ContextBuilder:    core,
		ConversationTurns: []domain.ConversationTurn{{Role: domain.ConversationRoleUser, Content: "上一轮问题"}},
	})
	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.Task{
		ID:    "task-session",
		Input: "继续回答",
	})
	if err != nil {
		t.Fatalf("RunTask(session context) error = %v, want nil", err)
	}
	for _, want := range []string{"Recent conversation:", "上一轮问题", "继续回答"} {
		if !strings.Contains(maas.prompt, want) {
			t.Fatalf("RunTask(session context) prompt missing %q:\n%s", want, maas.prompt)
		}
	}
}

func TestRuntimeExecutesModelToolCallsAndContinuesInference(t *testing.T) {
	t.Parallel()

	maas := &toolCallingMaas{}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"lookup"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "lookup",
		Description: "lookup test data",
		InputSchema: map[string]any{
			"required": []string{"query"},
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
		},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: call.ID, Success: true, Output: "cache is implemented by map"}, nil
	}))
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:   maas,
		Audit:  audit,
		Events: events,
		Tools:  registry,
	})

	run, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.Task{
		ID:    "task-tools",
		Input: "how does cache work",
	})
	if err != nil {
		t.Fatalf("RunTask(tool call) error = %v, want nil", err)
	}
	if run.Result != "cache uses map" {
		t.Fatalf("RunTask(tool call).Result = %q, want final answer", run.Result)
	}
	if len(maas.prompts) != 2 {
		t.Fatalf("toolCallingMaas prompts = %d, want 2", len(maas.prompts))
	}
	if len(maas.tools) == 0 || maas.tools[0].Name != "lookup" {
		t.Fatalf("first inference tools = %#v, want lookup descriptor", maas.tools)
	}
	if !strings.Contains(maas.prompts[1], "cache is implemented by map") {
		t.Fatalf("second inference prompt missing tool result:\n%s", maas.prompts[1])
	}
	runtimeEvents := mustRuntimeEvents(t, events)
	if !hasRuntimeEvent(runtimeEvents, "tool_executed") {
		t.Fatalf("runtime events missing tool_executed: %#v", runtimeEvents)
	}
	auditEvents := mustAuditEvents(t, audit)
	if !hasRuntimeAuditAction(auditEvents, "tool_executed") {
		t.Fatalf("audit events missing tool_executed: %#v", auditEvents)
	}
}

func TestRuntimeSupportsMultipleToolRounds(t *testing.T) {
	t.Parallel()

	maas := &multiRoundToolCallingMaas{}
	audit := adapter.NewMemoryAuditLog()
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"lookup"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "lookup",
		Description: "lookup test data",
		InputSchema: map[string]any{
			"required": []string{"query"},
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
		},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		outputs := map[string]string{
			"cache":    "cache is implemented by map",
			"eviction": "eviction uses LRU",
		}
		return domain.ToolResult{CallID: call.ID, Success: true, Output: outputs[call.Arguments["query"]]}, nil
	}))
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:  maas,
		Audit: audit,
		Tools: registry,
	})

	run, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.Task{
		ID:    "task-multi-round-tools",
		Input: "how does cache eviction work",
	})
	if err != nil {
		t.Fatalf("RunTask(multi-round tool call) error = %v, want nil", err)
	}
	if run.Result != "cache uses map with LRU eviction" {
		t.Fatalf("RunTask(multi-round tool call).Result = %q, want final answer", run.Result)
	}
	if len(maas.prompts) != 3 {
		t.Fatalf("multiRoundToolCallingMaas prompts = %d, want 3", len(maas.prompts))
	}
	if !strings.Contains(maas.prompts[1], "cache is implemented by map") {
		t.Fatalf("second inference prompt missing first tool result:\n%s", maas.prompts[1])
	}
	if !strings.Contains(maas.prompts[2], "eviction uses LRU") {
		t.Fatalf("third inference prompt missing second tool result:\n%s", maas.prompts[2])
	}
}

func TestRuntimeFeedsToolExecuteErrorBackToModel(t *testing.T) {
	t.Parallel()

	maas := &toolCallingMaas{}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"lookup"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "lookup",
		Description: "lookup test data",
		InputSchema: map[string]any{
			"required": []string{"query"},
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
		},
	}, tool.HandlerFunc(func(_ context.Context, _ domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, errors.New("task \"task-tool-error\" not found")
	}))
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:   maas,
		Audit:  audit,
		Events: events,
		Tools:  registry,
	})

	run, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.Task{
		ID:    "task-tool-error",
		Input: "how does cache work",
	})
	if err != nil {
		t.Fatalf("RunTask(tool execute error) error = %v, want nil", err)
	}
	if run.Result != "cache uses map" {
		t.Fatalf("RunTask(tool execute error).Result = %q, want final answer", run.Result)
	}
	if len(maas.prompts) != 2 {
		t.Fatalf("toolCallingMaas prompts = %d, want 2", len(maas.prompts))
	}
	if !strings.Contains(maas.prompts[1], "failed: task \"task-tool-error\" not found") {
		t.Fatalf("second inference prompt missing tool failure text:\n%s", maas.prompts[1])
	}
	runtimeEvents := mustRuntimeEvents(t, events)
	if !hasRuntimeEvent(runtimeEvents, "tool_failed") {
		t.Fatalf("runtime events missing tool_failed: %#v", runtimeEvents)
	}
}

func TestRuntimeInterruptStopsBeforeInference(t *testing.T) {
	t.Parallel()

	maas := &captureMaas{response: "should not run"}
	events := adapter.NewMemoryEventBus()
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:   maas,
		Audit:  adapter.NewMemoryAuditLog(),
		Events: events,
	})
	runner.Interrupt()

	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, domain.Task{
		ID:    "task-interrupt",
		Input: "do not call maas",
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("RunTask(interrupted) error = %v, want ErrInterrupted", err)
	}
	if maas.prompt != "" {
		t.Fatalf("RunTask(interrupted) MaaS prompt = %q, want no inference call", maas.prompt)
	}
	runtimeEvents := mustRuntimeEvents(t, events)
	if !hasLearningRuntimeEvent(runtimeEvents, evolution.SignalFailure) {
		t.Fatalf("RunTask(interrupted) events missing failure learning event: %#v", runtimeEvents)
	}
	if !hasLearningMessagePart(runtimeEvents, "reason="+evolution.FailureReasonInterrupted) {
		t.Fatalf("RunTask(interrupted) learning event missing interrupted reason: %#v", runtimeEvents)
	}
	if !hasLearningMessagePart(runtimeEvents, "lightweight=true") {
		t.Fatalf("RunTask(interrupted) learning event missing lightweight=true: %#v", runtimeEvents)
	}
}

type failingMaas struct{}

func (failingMaas) Generate(context.Context, port.InferenceRequest) (port.InferenceResponse, error) {
	return port.InferenceResponse{}, errors.New("inference unavailable")
}

// cancelSignalMaas blocks its inference until the context is cancelled, then
// returns ctx.Err(), standing in for a provider call stopped mid-flight. It
// closes started once it is actually inside Generate so a test can cancel only
// after the run has reached the inference call.
type cancelSignalMaas struct{ started chan struct{} }

func (m *cancelSignalMaas) Generate(ctx context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	close(m.started)
	<-ctx.Done()
	return port.InferenceResponse{}, ctx.Err()
}

// TestRuntimeRunTaskPropagatesContextCanceled pins risk point 2 of the
// interrupt feature: a cancelled context must surface out of RunTask as an
// error that errors.Is(context.Canceled), so the coordinator's cancel 分流
// (errors.Is(err, context.Canceled)) matches and lands the task in Cancelled.
// If the runtime ever wrapped ctx.Canceled behind its own sentinel with %v
// instead of %w, this fails.
func TestRuntimeRunTaskPropagatesContextCanceled(t *testing.T) {
	t.Parallel()

	maas := &cancelSignalMaas{started: make(chan struct{})}
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:   maas,
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := runner.RunTask(ctx, domain.Agent{ID: "a1"}, domain.Task{ID: "t1", Input: "x"})
		errCh <- err
	}()

	<-maas.started
	cancel()
	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTask on cancelled ctx err = %v, want errors.Is(context.Canceled)", err)
	}
}

type captureMaas struct {
	response  string
	reasoning string
	prompt    string

	// Usage fields let tests assert that token counts flow from the
	// InferenceResponse through loopState into persisted audit/events.
	// Zero by default, so existing captureMaas callers are unaffected.
	promptTokens     int
	completionTokens int
	cachedTokens     int
	totalTokens      int
}

type toolCallingMaas struct {
	prompts []string
	tools   []port.InferenceTool
}

type multiRoundToolCallingMaas struct {
	prompts []string
}

func (m *toolCallingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompts = append(m.prompts, testsupport.RequestText(req))
	if len(m.prompts) == 1 {
		m.tools = append([]port.InferenceTool(nil), req.Tools...)
		return port.InferenceResponse{
			ToolCalls: []domain.ToolCall{{
				ID:        "lookup-1",
				Name:      "lookup",
				Arguments: map[string]string{"query": "cache"},
			}},
		}, nil
	}
	return port.InferenceResponse{Text: "cache uses map"}, nil
}

func (m *multiRoundToolCallingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompts = append(m.prompts, testsupport.RequestText(req))
	switch len(m.prompts) {
	case 1:
		return port.InferenceResponse{
			ToolCalls: []domain.ToolCall{{
				ID:        "lookup-1",
				Name:      "lookup",
				Arguments: map[string]string{"query": "cache"},
			}},
		}, nil
	case 2:
		return port.InferenceResponse{
			ToolCalls: []domain.ToolCall{{
				ID:        "lookup-2",
				Name:      "lookup",
				Arguments: map[string]string{"query": "eviction"},
			}},
		}, nil
	default:
		return port.InferenceResponse{Text: "cache uses map with LRU eviction"}, nil
	}
}

// compactingToolMaas is a port.MaasInferenceClient test double that serves two
// distinct request shapes from a single fake: the tool-loop's own generate
// calls (returning a tool call for the first maxToolRounds calls, then a
// final text answer), and compactConversation's summarization calls, which it
// recognises by shape (a single RoleUser message built by summarizePrompt) and
// counts separately in summarizeCalls so tests can assert the compaction
// trigger fired without exceeding maxCompactionsPerTask.
type compactingToolMaas struct {
	maxToolRounds  int
	promptTokens   int
	toolCalls      int
	summarizeCalls int
}

func (m *compactingToolMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	if len(req.Messages) == 1 && strings.Contains(req.Messages[0].Content, "压缩") {
		m.summarizeCalls++
		return port.InferenceResponse{Text: "SUMMARY-TEXT"}, nil
	}
	m.toolCalls++
	if m.toolCalls <= m.maxToolRounds {
		return port.InferenceResponse{
			PromptTokens: m.promptTokens,
			ToolCalls: []domain.ToolCall{{
				ID:        fmt.Sprintf("call-%d", m.toolCalls),
				Name:      "lookup",
				Arguments: map[string]string{"query": fmt.Sprintf("q%d", m.toolCalls)},
			}},
		}, nil
	}
	return port.InferenceResponse{PromptTokens: m.promptTokens, Text: "final answer"}, nil
}

func newCompactionTestRuntime(t *testing.T, maas *compactingToolMaas, compactTokenThreshold int) *Runtime {
	t.Helper()
	audit := adapter.NewMemoryAuditLog()
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"lookup"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "lookup",
		Description: "lookup test data",
		InputSchema: map[string]any{
			"required": []string{"query"},
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
		},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: call.ID, Success: true, Output: "ok " + call.Arguments["query"]}, nil
	}))
	return NewRuntime(Config{Gate: NewTaskGate(),
		Maas:                  maas,
		Audit:                 audit,
		Tools:                 registry,
		MaxToolRounds:         15,
		CompactTokenThreshold: compactTokenThreshold,
	})
}

// TestRunToolLoopCompactsOverThreshold drives enough tool-calling rounds (each
// contributing promptTokens well over the small threshold) that runToolLoop's
// compaction trigger fires. It asserts compaction happened (a summarization
// call was made, distinguishable by request shape) and that it never ran more
// than maxCompactionsPerTask times, even though promptTokens stays over
// threshold for every remaining round.
func TestRunToolLoopCompactsOverThreshold(t *testing.T) {
	t.Parallel()

	maas := &compactingToolMaas{maxToolRounds: 10, promptTokens: 20}
	runner := newCompactionTestRuntime(t, maas, 5)

	run, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.Task{
		ID:    "task-compaction",
		Input: "keep looking things up",
	})
	if err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}
	if run.Result != "final answer" {
		t.Fatalf("RunTask.Result = %q, want final answer", run.Result)
	}
	if maas.summarizeCalls == 0 {
		t.Fatalf("expected conversation compaction to trigger at least once, summarizeCalls=0")
	}
	if maas.summarizeCalls > maxCompactionsPerTask {
		t.Fatalf("compaction ran %d times, want <= maxCompactionsPerTask(%d)", maas.summarizeCalls, maxCompactionsPerTask)
	}
}

// TestRunToolLoopCompactionDisabledAtZeroThreshold uses the same tool-round
// workload as TestRunToolLoopCompactsOverThreshold but leaves
// CompactTokenThreshold at its zero value, which must disable compaction
// entirely (0 = off, not a fallback threshold).
func TestRunToolLoopCompactionDisabledAtZeroThreshold(t *testing.T) {
	t.Parallel()

	maas := &compactingToolMaas{maxToolRounds: 10, promptTokens: 20}
	runner := newCompactionTestRuntime(t, maas, 0)

	if _, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.Task{
		ID:    "task-no-compaction",
		Input: "keep looking things up",
	}); err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}
	if maas.summarizeCalls != 0 {
		t.Fatalf("compact_token_threshold=0 must disable compaction, got summarizeCalls=%d", maas.summarizeCalls)
	}
}

func (m *captureMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompt = testsupport.RequestText(req)
	return port.InferenceResponse{
		Text:             m.response,
		ReasoningSummary: m.reasoning,
		PromptTokens:     m.promptTokens,
		CompletionTokens: m.completionTokens,
		CachedTokens:     m.cachedTokens,
		TotalTokens:      m.totalTokens,
	}, nil
}

// mustAuditEvents reads the audit log's events, failing the test immediately
// if the read itself errors (fail-loud: never silently substitute an empty
// slice for a read failure).
func mustAuditEvents(t *testing.T, log port.AuditLog) []domain.AuditEvent {
	t.Helper()
	events, err := log.Events()
	if err != nil {
		t.Fatalf("AuditLog.Events() error = %v", err)
	}
	return events
}

// mustRuntimeEvents reads the event bus's events, failing the test immediately
// if the read itself errors.
func mustRuntimeEvents(t *testing.T, bus port.EventBus) []domain.RuntimeEvent {
	t.Helper()
	events, err := bus.Events()
	if err != nil {
		t.Fatalf("EventBus.Events() error = %v", err)
	}
	return events
}

func hasLearningRuntimeEvent(events []domain.RuntimeEvent, signal evolution.SignalKind) bool {
	for _, event := range events {
		if event.Type == evolution.RuntimeEventLearning && strings.Contains(event.Message, "signal="+string(signal)) {
			return true
		}
	}
	return false
}

func hasLearningMessagePart(events []domain.RuntimeEvent, part string) bool {
	for _, event := range events {
		if event.Type == evolution.RuntimeEventLearning && strings.Contains(event.Message, part) {
			return true
		}
	}
	return false
}

// budgetExhaustingMaas always asks for another tool while tools are offered, and
// only returns a textual answer once no tools are offered (the forced final
// pass). It models a task that exhausts its tool-round budget.
type budgetExhaustingMaas struct {
	toolRounds  int
	forcedNoOps bool
}

func (m *budgetExhaustingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	if len(req.Tools) == 0 {
		m.forcedNoOps = true
		return port.InferenceResponse{Text: "best-effort answer from gathered info"}, nil
	}
	m.toolRounds++
	return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
		ID:        "lookup-loop",
		Name:      "lookup",
		Arguments: map[string]string{"query": "more"},
	}}}, nil
}

func TestRuntimeGracefullyAnswersWhenToolBudgetExhausted(t *testing.T) {
	t.Parallel()

	maas := &budgetExhaustingMaas{}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"lookup"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "lookup",
		Description: "lookup test data",
		InputSchema: map[string]any{
			"required":   []string{"query"},
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
		},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: call.ID, Success: true, Output: "partial data"}, nil
	}))
	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:          maas,
		Audit:         audit,
		Events:        events,
		Tools:         registry,
		MaxToolRounds: 2,
	})

	run, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.Task{
		ID:    "task-budget",
		Input: "explore everything",
	})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want graceful success after tool budget exhausted", err)
	}
	if !maas.forcedNoOps {
		t.Fatal("expected a final no-tools inference to force an answer, but it never happened")
	}
	if run.Result != "best-effort answer from gathered info" {
		t.Fatalf("RunTask().Result = %q, want the forced final answer", run.Result)
	}
	runtimeEvents := mustRuntimeEvents(t, events)
	if !hasRuntimeEvent(runtimeEvents, "task_completed") {
		t.Fatalf("runtime events missing task_completed: %#v", runtimeEvents)
	}
}

// TestLazyToolHintPointsToLoadCapabilitiesNotListTools guards against
// regressing to the deleted list_tools meta tool. The hint text is spliced
// directly into the prompt sent to the model (buildPrompt), so if it ever
// tells the model to call list_tools again the model will call a tool that
// dispatchToolCall does not recognize, aborting the task with "unhandled
// meta tool" (see internal/runtime/lazytools.go dispatchToolCall).
func TestLazyToolHintPointsToLoadCapabilitiesNotListTools(t *testing.T) {
	t.Parallel()

	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:      adapter.NewRecordingMaas("done"),
		LazyTools: true,
	})

	hint := runner.lazyToolHint([]string{"lookup"})

	if strings.Contains(hint, "list_tools") {
		t.Fatalf("lazyToolHint() = %q, must not reference the deleted list_tools meta tool", hint)
	}
	if !strings.Contains(hint, "load_capabilities") {
		t.Fatalf("lazyToolHint() = %q, want it to guide the model to load_capabilities", hint)
	}
}

// alternatingRepeatMaas requests two distinct tool calls (A, B) in strict
// alternation — A,B,A,B,... — so no two consecutive rounds ever repeat the
// same call (repeatedCallStreak stays at 1), while each of A and B
// individually recurs across the whole task. It models the 152-incident's
// non-consecutive counterpart: a model that oscillates between two calls
// instead of hammering one. Once no tools are offered (the forced final
// pass) it returns a plain textual answer.
type alternatingRepeatMaas struct {
	rounds int
}

func (m *alternatingRepeatMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	if len(req.Tools) == 0 {
		return port.InferenceResponse{Text: "final answer after alternating repeats"}, nil
	}
	call := domain.ToolCall{ID: "call-a", Name: "lookup", Arguments: map[string]string{"query": "cache"}}
	if m.rounds%2 == 1 {
		call = domain.ToolCall{ID: "call-b", Name: "other", Arguments: map[string]string{"query": "eviction"}}
	}
	m.rounds++
	return port.InferenceResponse{ToolCalls: []domain.ToolCall{call}}, nil
}

// TestRunToolLoopAbortsOnNonConsecutiveRepeat guards the non-consecutive
// repeat guard (repeatGuard, messages.go): a model that alternates between two
// distinct tool calls (A,B,A,B,...) never trips the consecutive-streak guard
// (repeatedCallStreak never reaches repeatAbortStreak), but each call
// individually accumulates across the task and must still trip the abort path
// once one of them recurs repeatAbortCount times.
func TestRunToolLoopAbortsOnNonConsecutiveRepeat(t *testing.T) {
	t.Parallel()

	maas := &alternatingRepeatMaas{}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"lookup", "other"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registerLookupLikeTool := func(name string) {
		registry.RegisterDescriptor(tool.Descriptor{
			Name:        name,
			Description: name + " test data",
			InputSchema: map[string]any{
				"required":   []string{"query"},
				"properties": map[string]any{"query": map[string]any{"type": "string"}},
			},
		}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{CallID: call.ID, Success: true, Output: "data for " + name}, nil
		}))
	}
	registerLookupLikeTool("lookup")
	registerLookupLikeTool("other")

	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:          maas,
		Audit:         audit,
		Events:        events,
		Tools:         registry,
		MaxToolRounds: 20,
	})

	run, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.Task{
		ID:    "task-alt-repeat",
		Input: "alternate between lookup and other",
	})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want graceful completion after the abort cuts the loop", err)
	}
	if run.Result != "final answer after alternating repeats" {
		t.Fatalf("RunTask().Result = %q, want the forced final answer after the loop was cut", run.Result)
	}
	runtimeEvents := mustRuntimeEvents(t, events)
	if !hasRuntimeEvent(runtimeEvents, "tool_loop_broken") {
		t.Fatalf("runtime events missing tool_loop_broken for a non-consecutive repeat: %#v", runtimeEvents)
	}
	// The loop must have stopped well before the consecutive-streak threshold:
	// alternating calls never run more than 1 round deep, so if it took
	// anywhere near repeatAbortStreak (8) rounds per call to trip, the
	// consecutive guard — not the non-consecutive one — would be responsible.
	if maas.rounds > repeatAbortCount*2+1 {
		t.Fatalf("alternatingRepeatMaas made %d tool-offering rounds before the loop was cut, want it to stop shortly after repeatAbortCount (%d) non-consecutive repeats of one call", maas.rounds, repeatAbortCount)
	}
}

func hasRuntimeEvent(events []domain.RuntimeEvent, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func hasRuntimeAuditAction(events []domain.AuditEvent, action string) bool {
	for _, event := range events {
		if event.Action == action {
			return true
		}
	}
	return false
}

// findAuditEvent returns the first audit event with the given action, and
// whether one was found.
func findAuditEvent(events []domain.AuditEvent, action string) (domain.AuditEvent, bool) {
	for _, event := range events {
		if event.Action == action {
			return event, true
		}
	}
	return domain.AuditEvent{}, false
}

// findRuntimeEvent returns the first runtime event with the given type,
// failing the test immediately if none is found.
func findRuntimeEvent(t *testing.T, events []domain.RuntimeEvent, eventType string) domain.RuntimeEvent {
	t.Helper()
	for _, event := range events {
		if event.Type == eventType {
			return event
		}
	}
	t.Fatalf("runtime events missing %s: %#v", eventType, events)
	return domain.RuntimeEvent{}
}

// writeFileCaptureMaas drives a fixed sequence of tool-call rounds so
// TestRuntimeCapturesGeneratedFilesFromWriteFile can exercise: two write_file
// calls to the same path (the second a duplicate), one write_file call whose
// handler fails, and one non-write_file tool call — before answering with
// plain text on the final round.
type writeFileCaptureMaas struct {
	prompts []string
}

func (m *writeFileCaptureMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompts = append(m.prompts, testsupport.RequestText(req))
	switch len(m.prompts) {
	case 1:
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "c1",
			Name:      "write_file",
			Arguments: map[string]string{"path": "out/a.txt", "content": "hi"},
		}}}, nil
	case 2:
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "c2",
			Name:      "lookup",
			Arguments: map[string]string{"query": "x"},
		}}}, nil
	case 3:
		// Duplicate of round 1's path: must not appear twice in GeneratedFiles.
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "c3",
			Name:      "write_file",
			Arguments: map[string]string{"path": "out/a.txt", "content": "hi again"},
		}}}, nil
	case 4:
		// The registered handler fails this one; a failed write_file must not
		// contribute to GeneratedFiles.
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "c4",
			Name:      "write_file",
			Arguments: map[string]string{"path": "out/bad.txt", "content": "boom"},
		}}}, nil
	default:
		return port.InferenceResponse{Text: "all done"}, nil
	}
}

// TestRuntimeCapturesGeneratedFilesFromWriteFile drives a full RunTask through
// a tool loop with (in order): a successful write_file, an unrelated
// successful tool call, a duplicate successful write_file to the same path,
// and a write_file whose handler fails. It asserts the finished task's
// GeneratedFiles — on both the returned domain.TaskRun and the task_completed
// domain.RuntimeEvent — contains exactly the unique successful write_file path,
// in first-seen order: the duplicate, the failure, and the non-write_file call
// must all be excluded.
func TestRuntimeCapturesGeneratedFilesFromWriteFile(t *testing.T) {
	t.Parallel()

	maas := &writeFileCaptureMaas{}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"write_file", "lookup"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "write_file",
		Description: "write test data",
		InputSchema: map[string]any{
			"required": []string{"path", "content"},
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
		},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		if call.Arguments["path"] == "out/bad.txt" {
			return domain.ToolResult{}, fmt.Errorf("simulated write failure for %s", call.Arguments["path"])
		}
		return domain.ToolResult{CallID: call.ID, Success: true, Output: "wrote " + call.Arguments["path"]}, nil
	}))
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "lookup",
		Description: "lookup test data",
		InputSchema: map[string]any{
			"required":   []string{"query"},
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
		},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: call.ID, Success: true, Output: "data"}, nil
	}))

	runner := NewRuntime(Config{Gate: NewTaskGate(),
		Maas:          maas,
		Audit:         audit,
		Events:        events,
		Tools:         registry,
		MaxToolRounds: 6,
	})

	run, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.Task{
		ID:    "task-generated-files",
		Input: "write some files",
	})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}

	if got := run.GeneratedFiles; len(got) != 1 || got[0] != "out/a.txt" {
		t.Fatalf("TaskRun.GeneratedFiles = %#v, want [\"out/a.txt\"]", got)
	}

	runtimeEvents := mustRuntimeEvents(t, events)
	completed := findRuntimeEvent(t, runtimeEvents, "task_completed")
	if got := completed.GeneratedFiles; len(got) != 1 || got[0] != "out/a.txt" {
		t.Fatalf("task_completed event GeneratedFiles = %#v, want [\"out/a.txt\"]", got)
	}
}
