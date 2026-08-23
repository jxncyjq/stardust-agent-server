package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/browser"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/plugin/loader"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/runtime"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/taskledger"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

type DemoResult struct {
	TaskID           string
	Result           string
	ReasoningSummary string
	Events           []domain.RuntimeEvent
	EventStream      []domain.RuntimeEvent
	AuditActions     []string
}

type App struct {
	// ledger records which owner must revoke which runtime resource. Static
	// assembly registers under the "app" owner; plugin instances get their own.
	ledger *lifecycle.Ledger

	// mu guards plugins. The loader is attached during serve assembly and read
	// by the `agent plugins` commands, which in an embedded host (the Wails
	// GUI) run on a different goroutine than the assembly did.
	mu sync.RWMutex
	// plugins is the loader serve assembly built, or nil when this process has
	// no plugin deployment configured. See Plugins.
	plugins *loader.Loader
}

type RunTaskOptions struct {
	TaskID        string
	Prompt        string
	Plain         bool
	Maas          port.MaasInferenceClient
	Events        port.EventBus
	Audit         port.AuditLog
	TaskSink      TaskSink
	ContextPrefix string
	AgentID       string
	Role          string
	CompanyID     string
	// Mode is the task's working mode (domain.ModeManual/ModePlan/ModeAuto).
	// It is carried onto the constructed domain.Task so the runtime can apply
	// Mode-specific behavior (e.g. Plan mode's read-only tool subset via
	// Runtime.effectiveTools). Empty means the runtime's own default applies.
	Mode string
	// WorkingDir is the host filesystem directory this task's tools are
	// sandboxed to. When non-empty it takes priority over ToolRoot as the
	// WorkspacePathGuard root (mirrors agentToolRoot's task.WorkingDir
	// priority in internal/runtime/agent_resolver.go), confining every tool
	// call to this directory. Empty falls back to ToolRoot.
	WorkingDir        string
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
	// Browser gates the built-in browser tools (browser_open/read/click/type/
	// close). Disabled by default: enabling requires a usable Chromium in the
	// runtime environment. When Enabled, RunTask constructs a browser runtime
	// and registers the tools onto the per-task registry; a construction
	// failure is fatal to the run (fail-loud), never silently skipped.
	Browser config.BrowserConfig
	// ToolGate gates each tool call for this run's runtime (approval flows,
	// e.g. the TUI's in-process synchronous 方案 Y gate — see
	// internal/tui.NewApprovalGate). Nil never suspends or blocks (Auto
	// behaviour): the runtime enforces at RunTask entry that a Manual-mode
	// task never reaches a nil gate (see runtime.ErrManualGateMissing), so
	// Mode == domain.ModeManual requires both ToolGate and Checkpoints set.
	ToolGate runtime.ToolGate
	// Checkpoints persists suspended tool-loop state so a task can resume
	// after a suspend. Manual mode requires it set even when ToolGate never
	// actually suspends (e.g. 方案 Y's ShouldSuspend is always false): the
	// runtime's Manual-mode invariant check (RunTask, see
	// runtime.ErrManualGateMissing) treats a nil checkpoint store as an
	// unwired gate regardless of whether that particular gate would ever use
	// it, because it cannot tell the two cases apart from the Config alone.
	Checkpoints *sessionstate.Store
	// DisabledTools names tools the default runtime's agent may not use
	// (deny-list), mirroring runtime.Config.DisabledTools. Each name must be a
	// known gateable tool (validated in RunTask; an unknown name is a config
	// error — CLAUDE.md §0 fail-loud — not a silent no-op). Empty disables
	// nothing.
	DisabledTools []string
}

type TaskSink interface {
	SaveTask(ctx context.Context, task domain.Task) error
}

func New() *App {
	return &App{ledger: lifecycle.NewLedger()}
}

// PluginResources reports the live revocable resources per owner. It answers
// "the plugin loaded but nothing happened" without reading code.
func (a *App) PluginResources() map[lifecycle.Owner][]string {
	return a.ledger.Snapshot()
}

// PluginLedger returns the lifecycle ledger every plugin activation files its
// revocation handles under — the same one PluginResources reports. Serve
// assembly needs it to build the plugin loader (loader.Config.Ledger); nothing
// else should file under it.
func (a *App) PluginLedger() *lifecycle.Ledger {
	return a.ledger
}

// Plugins returns the plugin loader this process assembled, or nil when no
// plugin deployment is configured (config's plugins.manifest is absent).
//
// A nil return is the documented "plugins are not enabled here" state, not a
// failure: callers must distinguish it rather than dereference it. It is also
// nil in a process that never ran serve assembly — the loader belongs to a
// running service, and there is no cross-process view of another serve's
// plugins.
func (a *App) Plugins() *loader.Loader {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.plugins
}

// SetPlugins attaches the plugin loader serve assembly built, so that
// Plugins — and the `agent plugins` commands reading it — report THIS
// process's plugins.
//
// It refuses a nil loader and a second attachment rather than accepting
// either: a nil would be indistinguishable from "plugins are not configured",
// and a replacement would leave the plugins mounted by the first loader
// running with nothing tracking them.
//
// The refusal is not permanent. A serve that shuts down drains its plugins and
// then calls ClearPlugins, which releases the slot, so the same process can
// build serve again and attach the new assembly's loader. The refusal therefore
// means "something is still mounted under another loader", not "this process
// has used up its one attachment".
func (a *App) SetPlugins(pluginLoader *loader.Loader) error {
	if pluginLoader == nil {
		return errors.New("set plugin loader: loader is nil; " +
			"a process with no plugin deployment must simply not call this")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.plugins != nil {
		return errors.New("set plugin loader: a loader is already attached; " +
			"the plugins the first one mounted would be left running with nothing tracking them")
	}
	a.plugins = pluginLoader
	return nil
}

// ClearPlugins detaches the loader SetPlugins attached, releasing the slot so a
// later assembly in the same process can attach its own. Clearing when nothing
// is attached is a no-op.
//
// It is the counterpart of serve's shutdown drain (cli.drainPlugins) and MUST
// be called only once that drain has actually unmounted everything: detaching a
// loader whose plugins are still mounted would leave them running with nothing
// tracking them — exactly the state SetPlugins' refusal of a second attachment
// exists to prevent. A drain that failed therefore does not clear, so the next
// SetPlugins still refuses and says why.
func (a *App) ClearPlugins() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.plugins = nil
}

func (a *App) RunDemo(ctx context.Context) (DemoResult, error) {
	maas := adapter.NewRecordingMaas("demo task completed")
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	runner := runtime.NewRuntime(runtime.Config{
		Maas:     maas,
		Audit:    audit,
		Events:   events,
		Tools:    tool.NewWorkspaceRegistry(".", audit),
		ToolRoot: ".",
		// This path runs one task and returns; it mounts no plugins and shares
		// no registry with a plugin loader, so the gate it registers that task
		// on is its own. It is still required, not optional: the field is what
		// makes "which boundary does this runtime's task belong to?" an answered
		// question at every construction site.
		Gate: taskgate.NewTaskGate(),
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
	auditEvents, err := audit.Events()
	if err != nil {
		return DemoResult{}, fmt.Errorf("read audit events: %w", err)
	}
	runtimeEvents, err := events.Events()
	if err != nil {
		return DemoResult{}, fmt.Errorf("read runtime events: %w", err)
	}
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
	// Fail-loud assembly-time validation (CLAUDE.md §0): a disabled_tools entry
	// that does not name a known gateable tool is a config error, not an inert
	// no-op — a typo would otherwise silently disable nothing.
	for _, name := range opts.DisabledTools {
		if !toolauth.IsGateable(name) {
			return DemoResult{}, fmt.Errorf("disabled_tools names unknown tool %q", name)
		}
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
	toolRoot := strings.TrimSpace(opts.WorkingDir)
	if toolRoot == "" {
		toolRoot = opts.ToolRoot
	}
	if toolRoot == "" {
		toolRoot = "."
	}
	homeDir := resolveHomeDir(taskLogger)
	tools := tool.NewWorkspaceRegistry(toolRoot, audit, tool.WithAgentsInjection(opts.ToolMaxFileChars, homeDir))
	tool.RegisterTaskLedgerTools(tools, opts.TaskLedger)
	tool.RegisterAgentMessageTools(tools, opts.MessageStore)
	tool.RegisterWebTools(tools, opts.WebTools)
	if opts.Browser.Enabled {
		brt, err := browser.NewRuntime(browser.RuntimeConfig{
			Headless:              opts.Browser.Headless,
			BinPath:               opts.Browser.BinPath,
			MaxElements:           opts.Browser.MaxElements,
			SnapshotRuneThreshold: opts.Browser.SnapshotRuneThreshold,
			SnapshotTTL:           time.Duration(opts.Browser.SnapshotTTLHours) * time.Hour,
			SnapshotArchiveDir:    opts.Browser.SnapshotArchiveDir,
			Extractor:             adapter.NewMaasSnapshotExtractor(maas),
		})
		if err != nil {
			return DemoResult{}, fmt.Errorf("init browser runtime: %w", err)
		}
		// RunTask is the one-shot path: App has no daemon lifecycle and runs a
		// single task per call, so the browser it launches is closed when that
		// task returns — otherwise every RunTask would leak a Chromium process.
		defer func() {
			if cerr := brt.Close(context.Background(), browser.CloseReq{}); cerr != nil {
				taskLogger.Warn("close browser runtime", "error", cerr)
			}
		}()
		tool.RegisterBrowserTools(tools, tool.BrowserToolOptions{Enabled: true, Runtime: brt, ToolRoot: toolRoot})
	}
	runner := runtime.NewRuntime(runtime.Config{
		Maas:              maas,
		Audit:             audit,
		Events:            events,
		ContextBuilder:    contextBuilder,
		Tools:             tools,
		ToolRoot:          toolRoot,
		MaxToolRounds:     opts.MaxToolRounds,
		LazyTools:         opts.LazyTools,
		ConversationTurns: opts.ConversationTurns,
		ToolGate:          opts.ToolGate,
		Checkpoints:       opts.Checkpoints,
		Logger:            taskLogger,
		DisabledTools:     opts.DisabledTools,
		// One-shot task, its own gate — see RunDemo above.
		Gate: taskgate.NewTaskGate(),
	})
	task := domain.Task{
		ID:         opts.TaskID,
		CompanyID:  opts.CompanyID,
		AgentID:    opts.AgentID,
		Mode:       opts.Mode,
		WorkingDir: opts.WorkingDir,
		Status:     domain.TaskRunning,
		Input:      opts.Prompt,
		CreatedAt:  time.Now(),
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
	auditEvents, err := audit.Events()
	if err != nil {
		return DemoResult{}, fmt.Errorf("read audit events: %w", err)
	}
	runtimeEvents, err := events.Events()
	if err != nil {
		return DemoResult{}, fmt.Errorf("read runtime events: %w", err)
	}
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

// resolveHomeDir returns the user's home directory, or "" with a warning when it
// cannot be determined.
//
// A missing home directory must not stop the agent from starting, so this does
// not return an error — but it must not be invisible either. With
// HOME/USERPROFILE unset (service accounts, containers, Windows services) the
// global ~/.stardust/agents.md silently stops loading, and the resident-path
// dedup seed (ResidentAgentsPaths) stops recognising it as already-in-context,
// so the tools may re-inject it.
// The behaviour changes; without this line the user only sees "my global
// conventions aren't taking effect" and has nothing to go on.
func resolveHomeDir(logger *slog.Logger) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		logger.Warn("resolve home directory",
			"component", "app",
			"consequence", "global agents.md will not be loaded",
			"error", err)
		return ""
	}
	return homeDir
}
