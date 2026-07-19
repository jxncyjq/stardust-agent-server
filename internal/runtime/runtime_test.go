package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

func TestRuntimeRunTaskCompletesThroughMaasPort(t *testing.T) {
	t.Parallel()

	maas := adapter.NewRecordingMaas("done")
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	runner := NewRuntime(Config{
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
	runner := NewRuntime(Config{
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

func TestRuntimeUsesNoopPortsWhenAuditAndEventsMissing(t *testing.T) {
	t.Parallel()

	maas := &captureMaas{response: "done without optional ports", reasoning: "reasoned through noop ports"}
	runner := NewRuntime(Config{Maas: maas})
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

func TestRuntimeMissingMaasReturnsErrMaasUnavailable(t *testing.T) {
	t.Parallel()

	runner := NewRuntime(Config{
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
	runner := NewRuntime(Config{
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
	runner := NewRuntime(Config{
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
	runner := NewRuntime(Config{
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
	runner := NewRuntime(Config{
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
	runner := NewRuntime(Config{
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
	runner := NewRuntime(Config{
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
	runner := NewRuntime(Config{
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

type captureMaas struct {
	response  string
	reasoning string
	prompt    string
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
	m.prompts = append(m.prompts, req.Prompt)
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
	m.prompts = append(m.prompts, req.Prompt)
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

func (m *captureMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompt = req.Prompt
	return port.InferenceResponse{Text: m.response, ReasoningSummary: m.reasoning}, nil
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
	runner := NewRuntime(Config{
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
