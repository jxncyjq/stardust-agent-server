package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/runtime"
	"github.com/stardust/legion-agent/internal/taskledger"
	"github.com/stardust/legion-agent/internal/tool"
)

type DemoResult struct {
	TaskID           string
	Result           string
	ReasoningSummary string
	Events           []domain.RuntimeEvent
	EventStream      []domain.RuntimeEvent
	AuditActions     []string
}

type App struct{}

type RunTaskOptions struct {
	TaskID            string
	Prompt            string
	Plain             bool
	Maas              port.MaasInferenceClient
	Events            port.EventBus
	Audit             port.AuditLog
	TaskSink          TaskSink
	ContextPrefix     string
	AgentID           string
	Role              string
	CompanyID         string
	Logger            *slog.Logger
	Metrics           *observability.MetricsRecorder
	ToolRoot          string
	ToolMaxFileChars  int
	TaskLedger        *taskledger.Ledger
	MessageStore      tool.AgentMessageStore
	MaxToolRounds     int
	LazyTools         bool
	ConversationTurns []domain.ConversationTurn
	WebTools          tool.WebToolOptions
}

type TaskSink interface {
	SaveTask(ctx context.Context, task domain.Task) error
}

func New() *App {
	return &App{}
}

func (a *App) RunDemo(ctx context.Context) (DemoResult, error) {
	maas := adapter.NewRecordingMaas("demo task completed")
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	runner := runtime.NewRuntime(runtime.Config{
		Maas:   maas,
		Audit:  audit,
		Events: events,
		Tools:  tool.NewWorkspaceRegistry(".", audit),
	})
	task := domain.Task{
		ID:        "demo-task",
		CompanyID: "demo-company",
		AgentID:   "demo-agent",
		Status:    domain.TaskRunning,
		Input:     "Introduce the Legion Agent runtime.",
	}
	episodic := memory.NewEpisodicMemoryStore(adapter.KeywordEmbeddingProvider{})
	if _, err := episodic.Add(ctx, domain.Agent{ID: "demo-agent"}, domain.Task{ID: "previous-task"}, "runtime demo uses scheduler, tool and audit context"); err != nil {
		return DemoResult{}, fmt.Errorf("add demo memory: %w", err)
	}
	matches, err := episodic.Search(ctx, task.Input, 3)
	if err != nil {
		return DemoResult{}, fmt.Errorf("prefetch demo memory: %w", err)
	}
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:      "memory_prefetched",
		TaskID:    task.ID,
		Message:   fmt.Sprintf("prefetched %d memory entries", len(matches)),
		CreatedAt: time.Now(),
	}); err != nil {
		return DemoResult{}, fmt.Errorf("publish memory prefetch event: %w", err)
	}
	run, err := runner.RunTask(ctx, domain.Agent{
		ID:        "demo-agent",
		CompanyID: "demo-company",
		Role:      "developer",
		Status:    domain.AgentActive,
	}, task)
	if err != nil {
		return DemoResult{}, fmt.Errorf("run demo task: %w", err)
	}
	registry := tool.NewRegistry(
		tool.NewStaticPolicy(tool.DecisionAllow),
		tool.NewRolePermissionEnforcer(map[string]bool{"developer:echo_tool": true}),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.Register("echo_tool", tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: "demo-tool-call", Success: true, Output: "echo ok"}, nil
	}))
	if _, err := registry.Execute(ctx, domain.Agent{ID: "demo-agent", Role: "developer"}, domain.ToolCall{
		ID:   "demo-tool-call",
		Name: "echo_tool",
	}); err != nil {
		return DemoResult{}, fmt.Errorf("execute demo tool: %w", err)
	}
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:      "tool_executed",
		TaskID:    task.ID,
		Message:   "echo_tool completed",
		CreatedAt: time.Now(),
	}); err != nil {
		return DemoResult{}, fmt.Errorf("publish tool event: %w", err)
	}
	auditEvents := audit.Events()
	runtimeEvents := events.Events()
	return DemoResult{
		TaskID:           task.ID,
		Result:           quality.SanitizeModelOutput(run.Result),
		ReasoningSummary: quality.SanitizeModelOutput(run.ReasoningSummary),
		Events:           runtimeEvents,
		EventStream:      eventStream(task.ID, runtimeEvents, auditEvents),
		AuditActions:     auditActions(auditEvents),
	}, nil
}

func (a *App) RunTask(ctx context.Context, opts RunTaskOptions) (DemoResult, error) {
	if opts.TaskID == "" {
		opts.TaskID = "cli-task"
	}
	if opts.AgentID == "" {
		opts.AgentID = "cli-agent"
	}
	if opts.CompanyID == "" {
		opts.CompanyID = "cli-company"
	}
	if opts.Role == "" {
		opts.Role = "developer"
	}
	if opts.Prompt == "" {
		return DemoResult{}, fmt.Errorf("prompt is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	taskLogger := observability.WithTaskID(observability.WithComponent(logger, "app"), opts.TaskID)
	taskLogger.Info("task run started")
	maas := opts.Maas
	if maas == nil {
		maas = adapter.NewRecordingMaas("task completed")
	}
	audit := opts.Audit
	if audit == nil {
		audit = adapter.NewMemoryAuditLog()
	}
	events := opts.Events
	if events == nil {
		events = adapter.NewMemoryEventBus()
	}
	var contextBuilder runtime.ContextBuilder
	if opts.ContextPrefix != "" || len(opts.ConversationTurns) > 0 {
		contextBuilder = cognitive.NewCore(cognitive.NoopCompressor{}).WithContextFiles(opts.ContextPrefix)
	}
	toolRoot := opts.ToolRoot
	if toolRoot == "" {
		toolRoot = "."
	}
	homeDir, _ := os.UserHomeDir()
	tools := tool.NewWorkspaceRegistry(toolRoot, audit, tool.WithAgentsInjection(opts.ToolMaxFileChars, homeDir))
	tool.RegisterTaskLedgerTools(tools, opts.TaskLedger)
	tool.RegisterAgentMessageTools(tools, opts.MessageStore)
	tool.RegisterWebTools(tools, opts.WebTools)
	runner := runtime.NewRuntime(runtime.Config{
		Maas:              maas,
		Audit:             audit,
		Events:            events,
		ContextBuilder:    contextBuilder,
		Tools:             tools,
		MaxToolRounds:     opts.MaxToolRounds,
		LazyTools:         opts.LazyTools,
		ConversationTurns: opts.ConversationTurns,
	})
	task := domain.Task{
		ID:        opts.TaskID,
		CompanyID: opts.CompanyID,
		AgentID:   opts.AgentID,
		Status:    domain.TaskRunning,
		Input:     opts.Prompt,
		CreatedAt: time.Now(),
	}
	if opts.Metrics != nil {
		opts.Metrics.IncTaskStatus("running")
	}
	if opts.TaskSink != nil {
		if err := opts.TaskSink.SaveTask(ctx, task); err != nil {
			return DemoResult{}, fmt.Errorf("save running task: %w", err)
		}
	}
	run, err := runner.RunTask(ctx, domain.Agent{
		ID:        opts.AgentID,
		CompanyID: opts.CompanyID,
		Role:      opts.Role,
		Status:    domain.AgentActive,
	}, task)
	if err != nil {
		if opts.Metrics != nil {
			opts.Metrics.IncTaskStatus("failed")
			opts.Metrics.IncModelCall("failed")
		}
		return DemoResult{}, fmt.Errorf("run task: %w", err)
	}
	if opts.Metrics != nil {
		opts.Metrics.IncTaskStatus("done")
		opts.Metrics.IncModelCall("success")
	}
	taskLogger.Info("task run completed")
	if opts.TaskSink != nil {
		task.Status = domain.TaskDone
		if err := opts.TaskSink.SaveTask(ctx, task); err != nil {
			return DemoResult{}, fmt.Errorf("save completed task: %w", err)
		}
	}
	auditEvents := audit.Events()
	runtimeEvents := events.Events()
	return DemoResult{
		TaskID:           task.ID,
		Result:           quality.SanitizeModelOutput(run.Result),
		ReasoningSummary: quality.SanitizeModelOutput(run.ReasoningSummary),
		Events:           runtimeEvents,
		EventStream:      eventStream(task.ID, runtimeEvents, auditEvents),
		AuditActions:     auditActions(auditEvents),
	}, nil
}

func auditActions(events []domain.AuditEvent) []string {
	actions := make([]string, 0, len(events))
	for _, event := range events {
		actions = append(actions, event.Action)
	}
	return actions
}

func eventStream(taskID string, events []domain.RuntimeEvent, auditEvents []domain.AuditEvent) []domain.RuntimeEvent {
	stream := make([]domain.RuntimeEvent, 0, len(events)+len(auditEvents))
	for _, event := range events {
		event.Message = quality.SanitizeModelOutput(event.Message)
		stream = append(stream, event)
	}
	for _, event := range auditEvents {
		stream = append(stream, domain.RuntimeEvent{
			Type:      "audit_recorded",
			TaskID:    taskID,
			Message:   event.Action,
			CreatedAt: event.CreatedAt,
		})
	}
	return stream
}
