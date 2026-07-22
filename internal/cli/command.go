package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/contextfiles"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/eventbridge"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/manualgate"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
	agentruntime "github.com/stardust/legion-agent/internal/runtime"
	"github.com/stardust/legion-agent/internal/server"
	"github.com/stardust/legion-agent/internal/service"
	"github.com/stardust/legion-agent/internal/sessioncache"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/skill"
	"github.com/stardust/legion-agent/internal/storage"
	"github.com/stardust/legion-agent/internal/task"
	"github.com/stardust/legion-agent/internal/taskledger"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/tui"
	"github.com/stardust/legion-agent/internal/version"
	"github.com/stardust/legion-agent/internal/workflow"
)

var commandTaskSeq atomic.Uint64

const defaultLogFilePath = "logs/agent.log"

func NewRoot(application *app.App, out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "agent",
		Short:         "Legion Agent runtime",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newRunCommand(application, out))
	cmd.AddCommand(newTUICommand(application, out))
	cmd.AddCommand(newServeCommand(out))
	cmd.AddCommand(newBackupCommand(out))
	cmd.AddCommand(newRestoreCommand(out))
	cmd.AddCommand(newDataCommand(out))
	cmd.AddCommand(newSkillCommand(out))
	cmd.AddCommand(newVersionCommand(out))
	cmd.AddCommand(newDoctorCommand(out))
	return cmd
}

func Execute(application *app.App, out io.Writer, args []string) error {
	root := NewRoot(application, out)
	root.SetArgs(NormalizeArgs(args))
	return root.Execute()
}

func NormalizeArgs(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return append([]string(nil), args[1:]...)
	}
	return append([]string(nil), args...)
}

func newRunCommand(application *app.App, out io.Writer) *cobra.Command {
	var demo bool
	var plain bool
	var prompt string
	var maasURL string
	var maasAPIKey string
	var maasProfile string
	var configPath string
	var noContextFiles bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run an agent task",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Context(), config.Options{Path: configPath})
			if err != nil {
				return err
			}
			var result app.DemoResult
			switch {
			case demo:
				result, err = application.RunDemo(cmd.Context())
			case prompt != "":
				persistent, closePersistent, err := persistentRunPorts(cmd.Context(), cfg)
				if err != nil {
					return err
				}
				defer closePersistent()
				maas, err := maasClientFromConfig(cfg.Maas, maasProfile, maasURL, maasAPIKey)
				if err != nil {
					return err
				}
				runDisplay := tuiDisplayConfig(cfg.Maas, maasProfile, maasURL)
				contextPrefix, err := buildRunContextPrefix(cmd.Context(), cfg, noContextFiles, runDisplay.ModelName)
				if err != nil {
					return err
				}
				taskLedger, err := newCommandTaskLedger(cfg)
				if err != nil {
					return err
				}
				logger, err := defaultLogger()
				if err != nil {
					return err
				}
				result, err = application.RunTask(cmd.Context(), app.RunTaskOptions{
					TaskID:           newCommandTaskID("cli-task"),
					Prompt:           prompt,
					Maas:             maas,
					Events:           persistent.events,
					Audit:            persistent.audit,
					TaskSink:         persistent.taskSink,
					ContextPrefix:    contextPrefix,
					Logger:           logger,
					Metrics:          observability.NewMetricsRecorder(nil),
					ToolRoot:         cfg.ContextFiles.Root,
					ToolMaxFileChars: cfg.ContextFiles.MaxFileChars,
					TaskLedger:       taskLedger,
					MessageStore:     persistent.messageStore,
					MaxToolRounds:    cfg.Runtime.MaxToolRounds,
					LazyTools:        cfg.Runtime.LazyTools,
					WebTools:         webToolOptions(cfg.Web),
				})
			default:
				err = fmt.Errorf("run requires --demo or --prompt")
			}
			if err != nil {
				return err
			}
			if plain {
				return printPlainRunResult(out, result)
			}
			program := tea.NewProgram(tui.NewModel(result), tea.WithOutput(out))
			_, err = program.Run()
			return err
		},
	}
	cmd.Flags().BoolVar(&demo, "demo", false, "run a demo task")
	cmd.Flags().BoolVar(&plain, "plain", false, "print non-interactive output")
	cmd.Flags().StringVar(&prompt, "prompt", "", "run a single task with this prompt")
	cmd.Flags().StringVar(&maasURL, "maas-url", "", "MaaS inference base URL")
	cmd.Flags().StringVar(&maasAPIKey, "maas-api-key", "", "MaaS API key")
	cmd.Flags().StringVar(&maasProfile, "maas-profile", "", "MaaS profile name")
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	cmd.Flags().BoolVar(&noContextFiles, "no-context-files", false, "disable AGENTS/SOUL/TOOLS/USER/MEMORY context file loading")
	return cmd
}

func newTUICommand(application *app.App, out io.Writer) *cobra.Command {
	var configPath string
	var maasURL string
	var maasAPIKey string
	var maasProfile string
	var noContextFiles bool
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Run the interactive Legion Agent TUI",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Context(), config.Options{Path: configPath})
			if err != nil {
				return err
			}
			persistent, closePersistent, err := persistentRunPorts(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer closePersistent()
			maas, err := maasClientFromConfig(cfg.Maas, maasProfile, maasURL, maasAPIKey)
			if err != nil {
				return err
			}
			display := tuiDisplayConfig(cfg.Maas, maasProfile, maasURL)
			contextPrefix, err := buildRunContextPrefix(cmd.Context(), cfg, noContextFiles, display.ModelName)
			if err != nil {
				return err
			}
			registry, err := loadServeAgentRegistry(cmd.Context(), cfg, configPath)
			if err != nil {
				return err
			}
			taskLedger, err := newCommandTaskLedger(cfg)
			if err != nil {
				return err
			}
			// Manual-mode approval wiring for the TUI (方案 Y): pendingCh/decisionCh
			// and the approvalGate they back are created once, here, and shared by
			// every task the session runs — both TUI entry points (runTUITask and
			// runMentionedTUIAgentTask, via tuiTaskRunConfig.ToolGate below) and the
			// InteractiveModel (via InteractiveConfig.ApprovalCh/DecisionCh) hold the
			// opposing ends of the same pair for the lifetime of the program. This is
			// safe to share because the TUI only ever runs one task at a time (see
			// InteractiveModel.running), so at most one approvalGate.Resolve call is
			// ever blocked waiting on decisionCh. checkpointStore is required
			// alongside the gate by the runtime's Manual-mode invariant (see
			// runtime.ErrManualGateMissing) even though 方案 Y's gate never actually
			// suspends a round — see tuiTaskRunConfig.Checkpoints's doc comment.
			pendingApprovalCh := make(chan tui.PendingApproval)
			approvalDecisionCh := make(chan tui.ApprovalDecision)
			approvalGate := tui.NewApprovalGate(pendingApprovalCh, approvalDecisionCh)
			workspaceRoot, workspaceRootWarning := sessionstate.ResolveWorkspaceRoot(cfg.Workspace.Root)
			if workspaceRootWarning != "" {
				logger, err := defaultLogger()
				if err != nil {
					return err
				}
				logger.Warn("workspace root fallback", "detail", workspaceRootWarning)
			}
			checkpointStore := sessionstate.NewStore(workspaceRoot)
			session := newTUISessionController(tuiSessionControllerConfig{
				Store:         persistent.sessionStore,
				Enabled:       cfg.Session.Enabled,
				CompanyID:     "cli-company",
				AgentID:       "cli-agent",
				ModelProfile:  firstNonEmpty(maasProfile, cfg.Maas.DefaultProfile),
				RecentTurns:   cfg.Session.DefaultRecentTurns,
				MaxTurnChars:  cfg.Session.MaxTurnChars,
				RestoreLatest: cfg.Session.RestoreLatestOnTUIStart,
				Cache:         newSessionContextCache(cfg.Session),
			})
			if err := session.Initialize(cmd.Context()); err != nil {
				return err
			}
			runner := func(ctx context.Context, prompt string, emit func(domain.RuntimeEvent)) (app.DemoResult, error) {
				return runTUITask(ctx, application, tuiTaskRunConfig{
					Config:               cfg,
					Registry:             registry,
					Prompt:               prompt,
					DefaultMaas:          maas,
					DefaultContextPrefix: contextPrefix,
					DefaultModelProfile:  firstNonEmpty(maasProfile, cfg.Maas.DefaultProfile),
					Events:               persistent.events,
					Audit:                persistent.audit,
					TaskSink:             persistent.taskSink,
					TaskLedger:           taskLedger,
					MessageStore:         persistent.messageStore,
					Emit:                 emit,
					Session:              session,
					ToolGate:             approvalGate,
					Checkpoints:          checkpointStore,
				})
			}
			colorProfile := parseTUIColorProfile(cfg.TUI.ColorProfile)
			renderer := lipgloss.NewRenderer(out, termenv.WithProfile(colorProfile))
			tuiTheme := tui.ThemeColors{
				Accent:   cfg.TUI.Theme.Accent,
				Accent2:  cfg.TUI.Theme.Accent2,
				Text:     cfg.TUI.Theme.Text,
				Dim:      cfg.TUI.Theme.Dim,
				Error:    cfg.TUI.Theme.Error,
				StatusFg: cfg.TUI.Theme.StatusFg,
				StatusBg: cfg.TUI.Theme.StatusBg,
				ShellBg:  cfg.TUI.Theme.ShellBg,
			}
			program := tea.NewProgram(tui.NewInteractiveModel(tui.InteractiveConfig{
				StreamingRunner: runner,
				SkillManager:    skill.NewDiskManager(cfg.Skills.InstallRoot, nil),
				SkillManagers:   tuiSkillManagers(cfg, registry),
				SessionManager:  session,
				TaskLedger:      taskLedger,
				MessageStore:    persistent.messageStore,
				AgentID:         "cli-agent",
				AgentNames:      registry.Names(),
				AgentName:       display.AgentName,
				ModelName:       display.ModelName,
				HidePrompt:      !cfg.TUI.ShowPrompt,
				HideThinking:    !cfg.TUI.ShowThinking,
				Renderer:        renderer,
				Theme:           tuiTheme,
				ApprovalCh:      pendingApprovalCh,
				DecisionCh:      approvalDecisionCh,
				// Parent of every per-task context the model creates: a
				// cancelled command context (Ctrl-C / shutdown) unwinds the
				// in-flight task and any Manual-mode approval blocked in
				// approvalGate.Resolve, instead of leaving them parked on a
				// keypress that will never come.
				Context: cmd.Context(),
			}), tea.WithOutput(out), tea.WithAltScreen(), tea.WithMouseCellMotion())
			_, err = program.Run()
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	cmd.Flags().StringVar(&maasURL, "maas-url", "", "MaaS inference base URL")
	cmd.Flags().StringVar(&maasAPIKey, "maas-api-key", "", "MaaS API key")
	cmd.Flags().StringVar(&maasProfile, "maas-profile", "", "MaaS profile name")
	cmd.Flags().BoolVar(&noContextFiles, "no-context-files", false, "disable AGENTS/SOUL/TOOLS/USER/MEMORY context file loading")
	return cmd
}

func newCommandTaskID(prefix string) string {
	seq := commandTaskSeq.Add(1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UTC().UnixNano(), seq)
}

func buildRunContextPrefix(ctx context.Context, cfg config.Config, noContextFiles bool, modelName string) (string, error) {
	if noContextFiles || !cfg.ContextFiles.Enabled {
		return "", nil
	}
	block, err := contextfiles.Load(ctx, contextfiles.Config{
		Enabled:      cfg.ContextFiles.Enabled,
		Root:         cfg.ContextFiles.Root,
		SoulPath:     cfg.ContextFiles.SoulPath,
		ToolsPath:    cfg.ContextFiles.ToolsPath,
		UserPath:     cfg.ContextFiles.UserPath,
		MemoryPath:   cfg.ContextFiles.MemoryPath,
		MaxFileChars: cfg.ContextFiles.MaxFileChars,
	})
	if err != nil {
		return "", err
	}
	rendered := block.Render()
	// Inject workspace root so the model knows what paths to use with file tools.
	if wsRoot := strings.TrimSpace(cfg.ContextFiles.Root); wsRoot != "" {
		absWS, absErr := filepath.Abs(wsRoot)
		if absErr == nil {
			wsRoot = absWS
		}
		var ctxNote strings.Builder
		ctxNote.WriteString("\n\n当前工作目录 (workspace root): ")
		ctxNote.WriteString(wsRoot)
		ctxNote.WriteString("\n使用 read_file / write_file / list_files / search_content 时，请使用相对路径（相对于 workspace root），或确认使用的绝对路径在 workspace root 之内。")
		// Tell the model the exact paths of every context file so it doesn't guess.
		ctxNote.WriteString("\n\n上下文文件实际路径（如需读取或更新这些文件，请使用以下路径）：")
		ctxNote.WriteString("\n- agents.md 常驻位置(按优先级): ~/.stardust/agents.md, <workspace>/agents.md, <workspace>/.stardust/agents.md")
		for _, pair := range []struct{ label, path string }{
			{"SOUL.md", cfg.ContextFiles.SoulPath},
			{"TOOLS.md", cfg.ContextFiles.ToolsPath},
			{"USER.md", cfg.ContextFiles.UserPath},
			{"MEMORY.md", cfg.ContextFiles.MemoryPath},
		} {
			if strings.TrimSpace(pair.path) != "" {
				ctxNote.WriteString("\n- ")
				ctxNote.WriteString(pair.label)
				ctxNote.WriteString(": ")
				ctxNote.WriteString(pair.path)
			}
		}
		rendered = rendered + ctxNote.String()
	}
	if model := strings.TrimSpace(modelName); model != "" && model != "recording" {
		rendered = rendered + "\n\n当前模型 (model): " + model
	}
	return rendered, nil
}

func maasClient(baseURL string, apiKey string) port.MaasInferenceClient {
	if baseURL == "" {
		return nil
	}
	return adapter.NewHTTPMaasClient(adapter.HTTPMaasConfig{
		BaseURL: baseURL,
		APIKey:  apiKey,
	})
}

func maasClientFromConfig(cfg config.MaasConfig, profile string, baseURL string, apiKey string) (port.MaasInferenceClient, error) {
	if baseURL != "" {
		return maasClient(baseURL, apiKey), nil
	}
	client, err := adapter.NewMaasClientFromProfile(cfg, profile)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func maasFactoryFromConfig(cfg config.MaasConfig) agentruntime.MaasRunnerFactory {
	return func(profile string) (agentruntime.MaasRunnerFactoryResult, error) {
		client, err := adapter.NewMaasClientFromProfile(cfg, profile)
		if err != nil {
			return agentruntime.MaasRunnerFactoryResult{}, err
		}
		display := tuiDisplayConfig(cfg, profile, "")
		return agentruntime.MaasRunnerFactoryResult{Client: client, ModelName: display.ModelName}, nil
	}
}

type tuiDisplay struct {
	AgentName string
	ModelName string
}

type tuiAgentPrompt struct {
	AgentID      string
	Prompt       string
	TaskID       string
	IncludeInbox bool
	Mentioned    bool
}

type tuiTaskRunConfig struct {
	Config               config.Config
	Registry             *agentregistry.Registry
	Prompt               string
	DefaultMaas          port.MaasInferenceClient
	DefaultContextPrefix string
	DefaultModelProfile  string
	Events               port.EventBus
	Audit                port.AuditLog
	TaskSink             app.TaskSink
	Emit                 func(domain.RuntimeEvent)
	Session              *tuiSessionController
	ConversationTurns    []domain.ConversationTurn
	TaskLedger           *taskledger.Ledger
	MessageStore         tool.AgentMessageStore
	// ToolGate gates Manual-mode sensitive tool calls for both TUI task
	// entry points (runTUITask and runMentionedTUIAgentTask must both wire
	// it — an @mention path that skipped it would let a Manual-mode agent
	// bypass approval). Wired once at `tui` command startup to the TUI's
	// 方案 Y in-process approval gate (see tui.NewApprovalGate); nil for the
	// plain `run`/demo paths, which never construct a tuiTaskRunConfig.
	ToolGate agentruntime.ToolGate
	// Checkpoints satisfies the runtime's Manual-mode invariant (see
	// runtime.ErrManualGateMissing): a Manual task requires it non-nil
	// alongside ToolGate even though 方案 Y's gate never actually suspends a
	// round (ShouldSuspend always false), because the runtime cannot tell
	// "no gate wired" apart from "gate that never suspends" from the Config
	// alone. It shares the same on-disk store the `serve` command's
	// manualgate flow uses, so it is otherwise inert here.
	Checkpoints *sessionstate.Store
}

func parseTUIAgentPrompt(input string) tuiAgentPrompt {
	prompt := strings.TrimSpace(input)
	if !strings.HasPrefix(prompt, "@") {
		return tuiAgentPrompt{Prompt: prompt}
	}
	withoutAt := strings.TrimPrefix(prompt, "@")
	agentID, rest, found := strings.Cut(withoutAt, " ")
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return tuiAgentPrompt{Prompt: prompt}
	}
	if !found {
		return tuiAgentPrompt{AgentID: agentID, Mentioned: true}
	}
	prompt, taskID, includeInbox := parseTUIAgentOptions(rest)
	return tuiAgentPrompt{AgentID: agentID, Prompt: prompt, TaskID: taskID, IncludeInbox: includeInbox, Mentioned: true}
}

func parseTUIAgentOptions(input string) (string, string, bool) {
	fields := strings.Fields(input)
	promptFields := make([]string, 0, len(fields))
	var taskID string
	var includeInbox bool
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if field == "--task" {
			if i+1 < len(fields) {
				taskID = strings.TrimSpace(fields[i+1])
				i++
			}
			continue
		}
		if value, ok := strings.CutPrefix(field, "--task="); ok {
			taskID = strings.TrimSpace(value)
			continue
		}
		if field == "--inbox" {
			includeInbox = true
			continue
		}
		promptFields = append(promptFields, field)
	}
	return strings.TrimSpace(strings.Join(promptFields, " ")), taskID, includeInbox
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func runTUITask(ctx context.Context, application *app.App, cfg tuiTaskRunConfig) (app.DemoResult, error) {
	parsed := parseTUIAgentPrompt(cfg.Prompt)
	if cfg.Session != nil {
		turns, err := cfg.Session.RecentTurns(ctx)
		if err != nil {
			return app.DemoResult{}, err
		}
		cfg.ConversationTurns = turns
	}
	agentID, modelProfile := tuiSessionTurnMetadata(cfg, parsed)
	userPrompt := parsed.Prompt
	if parsed.Mentioned {
		result, err := runMentionedTUIAgentTask(ctx, application, cfg, parsed)
		if err != nil {
			return app.DemoResult{}, err
		}
		if cfg.Session != nil {
			if err := cfg.Session.RecordExchange(ctx, result.TaskID, agentID, modelProfile, userPrompt, result.Result); err != nil {
				return app.DemoResult{}, err
			}
		}
		return result, nil
	}
	events := newStreamingEventBus(cfg.Events, cfg.Emit)
	logger, err := defaultLogger()
	if err != nil {
		return app.DemoResult{}, err
	}
	result, err := application.RunTask(ctx, app.RunTaskOptions{
		TaskID:            newCommandTaskID("tui-task"),
		Prompt:            parsed.Prompt,
		Maas:              cfg.DefaultMaas,
		Events:            events,
		Audit:             cfg.Audit,
		TaskSink:          cfg.TaskSink,
		ContextPrefix:     cfg.DefaultContextPrefix,
		Mode:              cfg.Session.CurrentMode(),
		WorkingDir:        cfg.Session.CurrentWorkingDir(),
		Logger:            logger,
		Metrics:           observability.NewMetricsRecorder(nil),
		ToolRoot:          cfg.Config.ContextFiles.Root,
		ToolMaxFileChars:  cfg.Config.ContextFiles.MaxFileChars,
		TaskLedger:        cfg.TaskLedger,
		MessageStore:      cfg.MessageStore,
		MaxToolRounds:     cfg.Config.Runtime.MaxToolRounds,
		LazyTools:         cfg.Config.Runtime.LazyTools,
		ConversationTurns: cfg.ConversationTurns,
		WebTools:          webToolOptions(cfg.Config.Web),
		ToolGate:          cfg.ToolGate,
		Checkpoints:       cfg.Checkpoints,
	})
	if err != nil {
		return app.DemoResult{}, err
	}
	if cfg.Session != nil {
		if err := cfg.Session.RecordExchange(ctx, result.TaskID, agentID, modelProfile, userPrompt, result.Result); err != nil {
			return app.DemoResult{}, err
		}
	}
	return result, nil
}

func runMentionedTUIAgentTask(ctx context.Context, application *app.App, cfg tuiTaskRunConfig, parsed tuiAgentPrompt) (app.DemoResult, error) {
	if cfg.Registry == nil {
		return app.DemoResult{}, fmt.Errorf("agent %q not configured", parsed.AgentID)
	}
	agentCfg, ok := cfg.Registry.Get(parsed.AgentID)
	if !ok {
		return app.DemoResult{}, fmt.Errorf("agent %q not configured", parsed.AgentID)
	}
	if strings.TrimSpace(parsed.Prompt) == "" {
		return app.DemoResult{}, fmt.Errorf("agent %q requires a prompt after the mention", parsed.AgentID)
	}
	maasResult, err := maasFactoryFromConfig(cfg.Config.Maas)(agentCfg.MaasProfile)
	if err != nil {
		return app.DemoResult{}, err
	}
	contextCfg := cfg.Config
	contextCfg.ContextFiles = agentCfg.ContextFiles
	if contextCfg.ContextFiles.Root == "" {
		contextCfg.ContextFiles.Root = cfg.Config.ContextFiles.Root
	}
	contextPrefix, err := buildRunContextPrefix(ctx, contextCfg, false, maasResult.ModelName)
	if err != nil {
		return app.DemoResult{}, err
	}
	if parsed.TaskID != "" {
		taskContext, err := buildTUITaskLedgerContext(ctx, cfg.TaskLedger, parsed.TaskID)
		if err != nil {
			return app.DemoResult{}, err
		}
		contextPrefix = joinContextBlocks(contextPrefix, taskContext)
	}
	var inboxMessageIDs []string
	if parsed.IncludeInbox {
		inboxContext, messageIDs, err := buildTUIAgentMessageInboxContext(ctx, cfg.MessageStore, firstNonEmpty(agentCfg.ID, parsed.AgentID))
		if err != nil {
			return app.DemoResult{}, err
		}
		inboxMessageIDs = messageIDs
		contextPrefix = joinContextBlocks(contextPrefix, inboxContext)
	}
	toolRoot := contextCfg.ContextFiles.Root
	if toolRoot == "" {
		toolRoot = cfg.Config.ContextFiles.Root
	}
	toolMaxFileChars := contextCfg.ContextFiles.MaxFileChars
	if toolMaxFileChars <= 0 {
		toolMaxFileChars = cfg.Config.ContextFiles.MaxFileChars
	}
	events := newStreamingEventBus(cfg.Events, cfg.Emit)
	logger, err := defaultLogger()
	if err != nil {
		return app.DemoResult{}, err
	}
	result, err := application.RunTask(ctx, app.RunTaskOptions{
		TaskID:            newCommandTaskID("tui-task"),
		Prompt:            parsed.Prompt,
		Maas:              maasResult.Client,
		Events:            events,
		Audit:             cfg.Audit,
		TaskSink:          cfg.TaskSink,
		ContextPrefix:     contextPrefix,
		AgentID:           firstNonEmpty(agentCfg.ID, parsed.AgentID),
		Role:              firstNonEmpty(agentCfg.Role, "developer"),
		Mode:              cfg.Session.CurrentMode(),
		WorkingDir:        cfg.Session.CurrentWorkingDir(),
		Logger:            logger,
		Metrics:           observability.NewMetricsRecorder(nil),
		ToolRoot:          toolRoot,
		ToolMaxFileChars:  toolMaxFileChars,
		TaskLedger:        cfg.TaskLedger,
		MessageStore:      cfg.MessageStore,
		MaxToolRounds:     cfg.Config.Runtime.MaxToolRounds,
		LazyTools:         cfg.Config.Runtime.LazyTools,
		ConversationTurns: cfg.ConversationTurns,
		WebTools:          webToolOptions(cfg.Config.Web),
		ToolGate:          cfg.ToolGate,
		Checkpoints:       cfg.Checkpoints,
	})
	if err != nil {
		return app.DemoResult{}, err
	}
	if len(inboxMessageIDs) > 0 {
		if err := markTUIAgentMessagesRead(ctx, cfg.MessageStore, inboxMessageIDs); err != nil {
			return app.DemoResult{}, err
		}
	}
	if parsed.TaskID != "" {
		agentID := firstNonEmpty(agentCfg.ID, parsed.AgentID)
		if err := appendTUITaskLedgerResult(ctx, cfg.TaskLedger, parsed.TaskID, agentID, result); err != nil {
			return app.DemoResult{}, err
		}
	}
	return result, nil
}

func buildTUIAgentMessageInboxContext(ctx context.Context, store tool.AgentMessageStore, agentID string) (string, []string, error) {
	if store == nil {
		return "", nil, fmt.Errorf("message store is not configured")
	}
	messages, err := store.ListAgentMessages(ctx, domain.AgentMessageQuery{
		ToAgentID: strings.TrimSpace(agentID),
		Status:    domain.AgentMessageUnread,
		Limit:     20,
	})
	if err != nil {
		return "", nil, fmt.Errorf("read agent inbox: %w", err)
	}
	if len(messages) == 0 {
		return "AgentMessage inbox context:\nno unread messages", nil, nil
	}
	var b strings.Builder
	messageIDs := make([]string, 0, len(messages))
	b.WriteString("AgentMessage inbox context:")
	for _, message := range messages {
		messageIDs = append(messageIDs, message.ID)
		fmt.Fprintf(&b, "\n- `%s` `%s` %s -> %s: %s",
			message.ID,
			message.Type,
			message.FromAgentID,
			message.ToAgentID,
			message.Summary,
		)
		if message.TaskID != "" {
			b.WriteString(" task=")
			b.WriteString(message.TaskID)
		}
		if message.Artifact != "" {
			b.WriteString(" artifact=")
			b.WriteString(message.Artifact)
		}
	}
	return b.String(), messageIDs, nil
}

func markTUIAgentMessagesRead(ctx context.Context, store tool.AgentMessageStore, messageIDs []string) error {
	if store == nil {
		return fmt.Errorf("message store is not configured")
	}
	now := time.Now().UTC()
	for _, messageID := range messageIDs {
		if err := store.MarkAgentMessageRead(ctx, messageID, now); err != nil {
			return fmt.Errorf("mark message %q read: %w", messageID, err)
		}
	}
	return nil
}

func buildTUITaskLedgerContext(ctx context.Context, ledger *taskledger.Ledger, taskID string) (string, error) {
	if ledger == nil {
		return "", fmt.Errorf("task ledger is not configured")
	}
	projection, err := ledger.Snapshot(ctx)
	if err != nil {
		return "", fmt.Errorf("read task ledger snapshot: %w", err)
	}
	taskMarkdown := strings.TrimSpace(projection.TaskMarkdown[taskID])
	if taskMarkdown == "" {
		return "", fmt.Errorf("task %q not found", taskID)
	}
	return "TaskLedger task context:\n" + taskMarkdown, nil
}

func appendTUITaskLedgerResult(ctx context.Context, ledger *taskledger.Ledger, taskID string, agentID string, result app.DemoResult) error {
	if ledger == nil {
		return fmt.Errorf("task ledger is not configured")
	}
	if _, err := ledger.Append(ctx, taskledger.Event{
		TaskID:         taskID,
		Type:           taskledger.EventResultAppended,
		From:           agentID,
		ActorAgentID:   agentID,
		CorrelationID:  result.TaskID,
		IdempotencyKey: "tui-agent-result:" + taskID + ":" + result.TaskID,
		Summary:        result.Result,
	}); err != nil {
		return fmt.Errorf("append task ledger result: %w", err)
	}
	if _, err := ledger.Rebuild(ctx); err != nil {
		return fmt.Errorf("rebuild task ledger projection: %w", err)
	}
	return nil
}

func joinContextBlocks(blocks ...string) string {
	joined := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if strings.TrimSpace(block) != "" {
			joined = append(joined, strings.TrimSpace(block))
		}
	}
	return strings.Join(joined, "\n\n")
}

func tuiSessionTurnMetadata(cfg tuiTaskRunConfig, parsed tuiAgentPrompt) (string, string) {
	if parsed.Mentioned && cfg.Registry != nil {
		if agentCfg, ok := cfg.Registry.Get(parsed.AgentID); ok {
			return firstNonEmpty(agentCfg.ID, parsed.AgentID), agentCfg.MaasProfile
		}
		return parsed.AgentID, ""
	}
	return "cli-agent", cfg.DefaultModelProfile
}

func configDir(configPath string) string {
	if strings.TrimSpace(configPath) == "" {
		return "."
	}
	return filepath.Dir(configPath)
}

func newCommandTaskLedger(cfg config.Config) (*taskledger.Ledger, error) {
	root := strings.TrimSpace(cfg.ContextFiles.Root)
	if root == "" {
		root = "."
	}
	allowedAgentIDs := []string{"cli-agent", "default-agent"}
	for agentID := range cfg.Agents {
		allowedAgentIDs = append(allowedAgentIDs, agentID)
	}
	ledger, err := taskledger.New(taskledger.Config{
		WorkspaceRoot:   root,
		IndexPath:       cfg.Tasks.IndexPath,
		Root:            cfg.Tasks.Root,
		ArchiveRoot:     cfg.Tasks.ArchiveRoot,
		MaxIndexLines:   cfg.Tasks.MaxIndexLines,
		MaxTaskLines:    cfg.Tasks.MaxTaskLines,
		MaxMessageChars: cfg.Tasks.MaxMessageChars,
		ActiveStatuses:  cfg.Tasks.ActiveStatuses,
		DoneStatuses:    cfg.Tasks.DoneStatuses,
		AllowedAgentIDs: allowedAgentIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("create task ledger: %w", err)
	}
	return ledger, nil
}

func loadServeAgentRegistry(ctx context.Context, cfg config.Config, configPath string) (*agentregistry.Registry, error) {
	return agentregistry.Load(ctx, cfg, configDir(configPath))
}

func tuiSkillManagers(cfg config.Config, registry *agentregistry.Registry) map[string]skill.Manager {
	if registry == nil {
		return nil
	}
	names := registry.Names()
	if len(names) == 0 {
		return nil
	}
	managers := make(map[string]skill.Manager, len(names))
	for _, name := range names {
		agentCfg, ok := registry.Get(name)
		if !ok {
			continue
		}
		root := firstNonEmpty(agentCfg.Skills.InstallRoot, cfg.Skills.InstallRoot)
		if root == "" {
			continue
		}
		managers[name] = skill.NewDiskManager(root, nil)
	}
	return managers
}

func tuiDisplayConfig(cfg config.MaasConfig, profile string, explicitBaseURL string) tuiDisplay {
	if explicitBaseURL != "" {
		return tuiDisplay{AgentName: "agent", ModelName: "custom-maas"}
	}
	selected := profile
	if selected == "" {
		selected = cfg.DefaultProfile
	}
	if selected != "" {
		if p, ok := cfg.Profiles[selected]; ok {
			model := strings.TrimSpace(p.Model)
			if model == "" {
				model = selected
			}
			return tuiDisplay{AgentName: selected, ModelName: model}
		}
		return tuiDisplay{AgentName: selected, ModelName: selected}
	}
	if cfg.BaseURL != "" {
		return tuiDisplay{AgentName: "agent", ModelName: "maas"}
	}
	return tuiDisplay{AgentName: "agent", ModelName: "recording"}
}

func printPlainRunResult(out io.Writer, result app.DemoResult) error {
	_, err := fmt.Fprintf(out, "task=%s result=%q events=%d audit_actions=%d\n", result.TaskID, result.Result, len(result.Events), len(result.AuditActions))
	return err
}

type runPorts struct {
	events       port.EventBus
	audit        port.AuditLog
	taskSink     app.TaskSink
	sessionStore conversationStore
	messageStore tool.AgentMessageStore
}

type streamingEventBus struct {
	primary port.EventBus
	emit    func(domain.RuntimeEvent)

	mu     sync.Mutex
	events []domain.RuntimeEvent
}

func newStreamingEventBus(primary port.EventBus, emit func(domain.RuntimeEvent)) *streamingEventBus {
	return &streamingEventBus{
		primary: primary,
		emit:    emit,
	}
}

func (b *streamingEventBus) Publish(ctx context.Context, event domain.RuntimeEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	b.events = append(b.events, event)
	b.mu.Unlock()
	if b.emit != nil {
		b.emit(event)
	}
	if b.primary != nil {
		return b.primary.Publish(ctx, event)
	}
	return nil
}

func (b *streamingEventBus) Events() ([]domain.RuntimeEvent, error) {
	if b.primary != nil {
		events, err := b.primary.Events()
		if err != nil {
			return nil, fmt.Errorf("read primary event bus: %w", err)
		}
		return events, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]domain.RuntimeEvent(nil), b.events...), nil
}

func persistentRunPorts(ctx context.Context, cfg config.Config) (runPorts, func(), error) {
	if cfg.Storage.Driver != "sqlite" {
		return runPorts{}, func() {}, nil
	}
	repo, err := storage.OpenSQLite(ctx, cfg.Storage.Path)
	if err != nil {
		return runPorts{}, func() {}, err
	}
	return runPorts{
			events:       storage.NewSQLiteEventBus(repo),
			audit:        storage.NewSQLiteAuditLog(repo),
			taskSink:     repo,
			sessionStore: repo,
			messageStore: repo,
		}, func() {
			closeRepositoryLogging(slog.Default(), repo, "persistent-run")
		}, nil
}

type conversationStore interface {
	SaveAgentSession(context.Context, domain.AgentSession) error
	LatestAgentSession(context.Context, string, string) (domain.AgentSession, bool, error)
	ListAgentSessions(context.Context, string, string) ([]domain.AgentSession, error)
	AppendConversationTurn(context.Context, domain.ConversationTurn) error
	ListConversationTurns(context.Context, string, int) ([]domain.ConversationTurn, error)
}

type tuiSessionControllerConfig struct {
	Store         conversationStore
	Enabled       bool
	CompanyID     string
	AgentID       string
	ModelProfile  string
	RecentTurns   int
	MaxTurnChars  int
	RestoreLatest bool
	Cache         sessionContextCache
}

type tuiSessionController struct {
	store         conversationStore
	enabled       bool
	companyID     string
	agentID       string
	modelProfile  string
	recentTurns   int
	maxTurnChars  int
	restoreLatest bool
	cache         sessionContextCache
	currentID     string
	// currentMode and currentWorkingDir mirror the AgentSession.Mode/WorkingDir
	// of the session identified by currentID, kept in sync by NewSession,
	// Initialize, SwitchSession, SetMode and SetWorkingDir so CurrentMode/
	// CurrentWorkingDir can answer without a store round trip.
	currentMode       string
	currentWorkingDir string
}

type sessionContextCache interface {
	Get(sessioncache.Key) ([]domain.ConversationTurn, bool)
	Put(sessioncache.Key, []domain.ConversationTurn)
	InvalidateSession(string)
}

func newTUISessionController(cfg tuiSessionControllerConfig) *tuiSessionController {
	return &tuiSessionController{
		store:         cfg.Store,
		enabled:       cfg.Enabled && cfg.Store != nil,
		companyID:     firstNonEmpty(cfg.CompanyID, "cli-company"),
		agentID:       firstNonEmpty(cfg.AgentID, "cli-agent"),
		modelProfile:  cfg.ModelProfile,
		recentTurns:   normalizeRecentTurns(cfg.RecentTurns),
		maxTurnChars:  normalizeMaxTurnCharsForSession(cfg.MaxTurnChars),
		restoreLatest: cfg.RestoreLatest,
		cache:         cfg.Cache,
	}
}

func newSessionContextCache(cfg config.SessionConfig) sessionContextCache {
	if !cfg.CacheEnabled {
		return nil
	}
	return sessioncache.NewMemoryCache(cfg.CacheMaxEntries)
}

func (c *tuiSessionController) Initialize(ctx context.Context) error {
	if c == nil || !c.enabled {
		return nil
	}
	if c.restoreLatest {
		session, ok, err := c.store.LatestAgentSession(ctx, c.companyID, c.agentID)
		if err != nil {
			return err
		}
		if ok {
			c.currentID = session.ID
			c.applySessionState(session)
			return nil
		}
	}
	_, err := c.NewSession(ctx)
	return err
}

func (c *tuiSessionController) CurrentSessionID() string {
	if c == nil {
		return ""
	}
	return c.currentID
}

func (c *tuiSessionController) NewSession(ctx context.Context) (string, error) {
	if c == nil || !c.enabled {
		return "", nil
	}
	now := time.Now()
	id := fmt.Sprintf("session-%d", now.UTC().UnixNano())
	session := domain.AgentSession{
		ID:        id,
		CompanyID: c.companyID,
		AgentID:   c.agentID,
		Title:     "TUI session",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := c.store.SaveAgentSession(ctx, session); err != nil {
		return "", err
	}
	c.invalidateCurrentSessionCache()
	c.currentID = id
	c.applySessionState(session)
	return id, nil
}

func (c *tuiSessionController) ListSessions(ctx context.Context) ([]string, error) {
	if c == nil || !c.enabled {
		return nil, nil
	}
	sessions, err := c.store.ListAgentSessions(ctx, c.companyID, c.agentID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(sessions))
	for _, session := range sessions {
		label := session.ID
		if session.Title != "" {
			label += "  " + session.Title
		}
		out = append(out, label)
	}
	return out, nil
}

func (c *tuiSessionController) SwitchSession(ctx context.Context, id string) error {
	if c == nil || !c.enabled {
		return nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("session id is required")
	}
	sessions, err := c.store.ListAgentSessions(ctx, c.companyID, c.agentID)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		if session.ID == id {
			c.invalidateCurrentSessionCache()
			c.currentID = id
			c.applySessionState(session)
			return nil
		}
	}
	return fmt.Errorf("session %q not found", id)
}

func (c *tuiSessionController) ClearSession(ctx context.Context) error {
	if c == nil || !c.enabled {
		return nil
	}
	_, err := c.NewSession(ctx)
	return err
}

func (c *tuiSessionController) RecentTurns(ctx context.Context) ([]domain.ConversationTurn, error) {
	if c == nil || !c.enabled {
		return nil, nil
	}
	if c.currentID == "" {
		if err := c.Initialize(ctx); err != nil {
			return nil, err
		}
	}
	if c.cache != nil {
		if turns, ok := c.cache.Get(c.cacheKey()); ok {
			return turns, nil
		}
	}
	turns, err := c.store.ListConversationTurns(ctx, c.currentID, c.recentTurns)
	if err != nil {
		return nil, err
	}
	for i := range turns {
		turns[i].Content = truncateSessionTurn(turns[i].Content, c.maxTurnChars)
	}
	if c.cache != nil {
		c.cache.Put(c.cacheKey(), turns)
	}
	return turns, nil
}

func (c *tuiSessionController) RecordExchange(ctx context.Context, taskID string, agentID string, modelProfile string, prompt string, result string) error {
	if c == nil || !c.enabled {
		return nil
	}
	if err := c.recordTurn(ctx, domain.ConversationRoleUser, taskID, agentID, modelProfile, prompt); err != nil {
		return err
	}
	return c.recordTurn(ctx, domain.ConversationRoleAssistant, taskID, agentID, modelProfile, result)
}

func (c *tuiSessionController) recordTurn(ctx context.Context, role domain.ConversationRole, taskID string, agentID string, modelProfile string, content string) error {
	if c == nil || !c.enabled {
		return nil
	}
	if c.currentID == "" {
		if err := c.Initialize(ctx); err != nil {
			return err
		}
	}
	now := time.Now()
	if err := c.store.AppendConversationTurn(ctx, domain.ConversationTurn{
		ID:           fmt.Sprintf("%s:%s:%d", c.currentID, role, now.UTC().UnixNano()),
		SessionID:    c.currentID,
		TaskID:       taskID,
		AgentID:      firstNonEmpty(agentID, c.agentID),
		ModelProfile: firstNonEmpty(modelProfile, c.modelProfile),
		Role:         role,
		Content:      truncateSessionTurn(content, c.maxTurnChars),
		CreatedAt:    now,
	}); err != nil {
		return err
	}
	c.invalidateCurrentSessionCache()
	return nil
}

func (c *tuiSessionController) cacheKey() sessioncache.Key {
	if c == nil {
		return sessioncache.Key{}
	}
	return sessioncache.Key{
		CompanyID:    c.companyID,
		AgentID:      c.agentID,
		SessionID:    c.currentID,
		ModelProfile: c.modelProfile,
		RecentTurns:  c.recentTurns,
		MaxTurnChars: c.maxTurnChars,
	}
}

func (c *tuiSessionController) invalidateCurrentSessionCache() {
	if c == nil || c.cache == nil || c.currentID == "" {
		return
	}
	c.cache.InvalidateSession(c.currentID)
}

// applySessionState mirrors session's Mode/WorkingDir onto currentMode /
// currentWorkingDir. It normalizes Mode via domain.NormalizeMode so an empty
// persisted value reads back as domain.ModeAuto, matching NormalizeMode's own
// "empty is a legitimate default" contract; a value NormalizeMode rejects (should
// not occur for state this controller itself wrote) is kept as-is rather than
// silently coerced, so a caller inspecting CurrentMode still sees the raw data.
func (c *tuiSessionController) applySessionState(session domain.AgentSession) {
	mode, ok := domain.NormalizeMode(session.Mode)
	if !ok {
		mode = session.Mode
	}
	c.currentMode = mode
	c.currentWorkingDir = session.WorkingDir
}

// currentAgentSession loads the full AgentSession record for currentID from
// the store. SaveAgentSession persists whole records (see
// storage.SQLiteRepository.SaveAgentSession), so mutating a single field
// requires reading the current record first or every other field would be
// clobbered back to a stale/zero value.
func (c *tuiSessionController) currentAgentSession(ctx context.Context) (domain.AgentSession, error) {
	sessions, err := c.store.ListAgentSessions(ctx, c.companyID, c.agentID)
	if err != nil {
		return domain.AgentSession{}, err
	}
	for _, session := range sessions {
		if session.ID == c.currentID {
			return session, nil
		}
	}
	return domain.AgentSession{}, fmt.Errorf("session %q not found", c.currentID)
}

// CurrentMode implements SessionManager. See SessionManager.CurrentMode.
func (c *tuiSessionController) CurrentMode() string {
	if c == nil {
		return domain.ModeAuto
	}
	mode, ok := domain.NormalizeMode(c.currentMode)
	if !ok {
		return c.currentMode
	}
	return mode
}

// SetMode implements SessionManager. See SessionManager.SetMode.
func (c *tuiSessionController) SetMode(ctx context.Context, mode string) error {
	if c == nil || !c.enabled {
		return fmt.Errorf("会话未启用，无法设置工作模式")
	}
	normalized, ok := domain.NormalizeMode(mode)
	if !ok {
		return fmt.Errorf("invalid mode %q (want manual|plan|auto)", mode)
	}
	if c.currentID == "" {
		if err := c.Initialize(ctx); err != nil {
			return err
		}
	}
	session, err := c.currentAgentSession(ctx)
	if err != nil {
		return err
	}
	session.Mode = normalized
	session.UpdatedAt = time.Now()
	if err := c.store.SaveAgentSession(ctx, session); err != nil {
		return err
	}
	c.invalidateCurrentSessionCache()
	c.currentMode = normalized
	return nil
}

// CurrentWorkingDir implements SessionManager. See SessionManager.CurrentWorkingDir.
func (c *tuiSessionController) CurrentWorkingDir() string {
	if c == nil {
		return ""
	}
	return c.currentWorkingDir
}

// SetWorkingDir implements SessionManager. See SessionManager.SetWorkingDir.
//
// This mirrors the HTTP server's PATCH /v1/sessions handler
// (handlePatchSession in server/http.go): set-once-then-immutable semantics
// on WorkingDir. Session on-disk state (sessionstate.SessionBase) is filed
// under whatever working_dir a session carries at write time, and repointing
// it strands previously-filed state -- silently, since recovery after a
// restart only enumerates bases currently in use. The TUI writes the same
// conversationStore record the HTTP server does, so it must enforce the same
// guard: once WorkingDir is non-empty, changing it to a different directory
// is rejected outright. Setting it for the first time (currently empty) and
// re-setting the same value (no-op) both remain allowed.
func (c *tuiSessionController) SetWorkingDir(ctx context.Context, dir string) error {
	if c == nil || !c.enabled {
		return fmt.Errorf("会话未启用，无法设置工作目录")
	}
	dir = strings.TrimSpace(dir)
	if dir != "" {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("stat working dir %q: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("working dir %q is not a directory", dir)
		}
	}
	if c.currentID == "" {
		if err := c.Initialize(ctx); err != nil {
			return err
		}
	}
	session, err := c.currentAgentSession(ctx)
	if err != nil {
		return err
	}
	currentWorkingDir := strings.TrimSpace(session.WorkingDir)
	if currentWorkingDir != "" && dir != currentWorkingDir {
		return fmt.Errorf("working_dir cannot be changed once set")
	}
	session.WorkingDir = dir
	session.UpdatedAt = time.Now()
	if err := c.store.SaveAgentSession(ctx, session); err != nil {
		return err
	}
	c.invalidateCurrentSessionCache()
	c.currentWorkingDir = dir
	return nil
}

func normalizeRecentTurns(turns int) int {
	if turns <= 0 {
		return 6
	}
	return turns
}

func normalizeMaxTurnChars(chars int) int {
	if chars <= 0 {
		return 6000
	}
	return chars
}

func normalizeMaxTurnCharsForSession(chars int) int {
	return normalizeMaxTurnChars(chars)
}

func truncateSessionTurn(value string, maxChars int) string {
	value = strings.TrimSpace(value)
	if maxChars <= 0 || len([]rune(value)) <= maxChars {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxChars]) + "\n[truncated]"
}

// parseTUIColorProfile converts a config string to a termenv.Profile.
//
// Accepted values (case-insensitive):
//
//	"truecolor" / "24bit"   → termenv.TrueColor  (default)
//	"ansi256"   / "256"     → termenv.ANSI256
//	"ansi"      / "16"      → termenv.ANSI
//	"ascii"     / "none"    → termenv.Ascii
func parseTUIColorProfile(s string) termenv.Profile {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ansi256", "256":
		return termenv.ANSI256
	case "ansi", "16":
		return termenv.ANSI
	case "ascii", "none", "no-color":
		return termenv.Ascii
	default:
		return termenv.TrueColor
	}
}

func newServeCommand(out io.Writer) *cobra.Command {
	var configPath string
	var addr string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Legion Agent service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Context().Err() != nil {
				_, err := fmt.Fprintln(out, "agent service stopped")
				return err
			}
			result, err := BuildServeService(cmd.Context(), ServeOptions{
				ConfigPath: configPath,
				Addr:       addr,
			})
			if err != nil {
				return err
			}
			defer result.Close()
			if err := result.Service.Start(cmd.Context()); err != nil {
				return err
			}
			_, err = fmt.Fprintln(out, "agent service stopped")
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	cmd.Flags().StringVar(&addr, "addr", "", "HTTP listen address")
	return cmd
}

// defaultLogger opens the process-wide file logger. It never falls back to a
// discarding logger: a silent downgrade to io.Discard mutes every later Warn and
// Error in the whole binary, which is exactly the failure that must be visible.
func defaultLogger() (*slog.Logger, error) {
	logger, err := newFileLogger(defaultLogFilePath)
	if err != nil {
		return nil, fmt.Errorf("open default logger at %q: %w", defaultLogFilePath, err)
	}
	return logger, nil
}

func newFileLogger(path string) (*slog.Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	logger, err := observability.NewLogger(&appendFileWriter{path: path}, observability.LoggerConfig{})
	if err != nil {
		return nil, err
	}
	return logger, nil
}

type appendFileWriter struct {
	path string
	mu   sync.Mutex
}

func (w *appendFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return file.Write(p)
}

type teeTaskStore struct {
	live       *task.Scheduler
	persistent server.TaskStore
}

// Add persists the new task and registers it with the live scheduler.
//
// The write to s.persistent is the ONLY thing that creates a task's row. The
// scheduler's own write-through moves an existing row's status and nothing
// else, and never inserts -- so removing this write would leave the tasks table
// permanently empty, and every later status update would find no row to change.
func (s teeTaskStore) Add(ctx context.Context, taskToAdd domain.Task) error {
	if err := s.live.Add(ctx, taskToAdd); err != nil {
		return err
	}
	if s.persistent == nil || s.persistent == s.live {
		return nil
	}
	return s.persistent.Add(ctx, taskToAdd)
}

func (s teeTaskStore) Get(ctx context.Context, taskID string) (domain.Task, bool, error) {
	if s.live != nil {
		taskToGet, ok, err := s.live.Get(ctx, taskID)
		if err != nil || ok {
			return taskToGet, ok, err
		}
	}
	if s.persistent == nil || s.persistent == s.live {
		return domain.Task{}, false, nil
	}
	return s.persistent.Get(ctx, taskID)
}

// List returns the live scheduler's tasks, i.e. the tasks submitted in the
// current serve session. Persisted history is intentionally not merged here: the
// status panel shows what this running instance is working on, and the live
// scheduler is the single source for that view.
func (s teeTaskStore) List(ctx context.Context) ([]domain.Task, error) {
	return s.live.List(ctx)
}

type memoryWorkflowStateStore struct {
	mu     sync.Mutex
	states map[string]storage.WorkflowState
}

func newMemoryWorkflowStateStore() *memoryWorkflowStateStore {
	return &memoryWorkflowStateStore{states: make(map[string]storage.WorkflowState)}
}

func (s *memoryWorkflowStateStore) ListWaitingWorkflowStates(ctx context.Context) ([]storage.WorkflowState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	states := make([]storage.WorkflowState, 0)
	for _, state := range s.states {
		if state.Result.Status == workflow.StatusWaitingApproval || state.Result.Status == workflow.StatusWaitingEvent {
			states = append(states, state)
		}
	}
	return states, nil
}

func (s *memoryWorkflowStateStore) SaveWorkflowState(ctx context.Context, def workflow.Definition, result workflow.Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[def.ID] = storage.WorkflowState{Definition: def, Result: result, UpdatedAt: time.Now()}
	return nil
}

func (s *memoryWorkflowStateStore) GetWorkflowState(ctx context.Context, workflowID string) (storage.WorkflowState, bool, error) {
	if err := ctx.Err(); err != nil {
		return storage.WorkflowState{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[workflowID]
	return state, ok, nil
}

func serviceStores(ctx context.Context, cfg config.Config) (server.TaskStore, server.WaitingWorkflowStore, server.SessionStore, server.ReadinessChecker, func(), error) {
	if cfg.Storage.Driver != "sqlite" {
		return task.NewScheduler(), newMemoryWorkflowStateStore(), nil, nil, func() {}, nil
	}
	repo, err := storage.OpenSQLite(ctx, cfg.Storage.Path)
	if err != nil {
		return nil, nil, nil, nil, func() {}, err
	}
	return repo, repo, repo, repo, func() {
		closeRepositoryLogging(slog.Default(), repo, "serve")
	}, nil
}

func newBackupCommand(out io.Writer) *cobra.Command {
	var configPath string
	var backupPath string
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up the configured SQLite database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Context(), config.Options{Path: configPath})
			if err != nil {
				return err
			}
			if cfg.Storage.Driver != "sqlite" {
				return fmt.Errorf("backup requires sqlite storage")
			}
			if backupPath == "" {
				return fmt.Errorf("backup requires --out")
			}
			manifest, err := storage.BackupSQLite(cmd.Context(), cfg.Storage.Path, backupPath)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(out, "backup=%s checksum=%s\n", manifest.BackupPath, manifest.Checksum)
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	cmd.Flags().StringVar(&backupPath, "out", "", "backup output path")
	return cmd
}

func newRestoreCommand(out io.Writer) *cobra.Command {
	var configPath string
	var backupPath string
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore the configured SQLite database from a backup",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Context(), config.Options{Path: configPath})
			if err != nil {
				return err
			}
			if cfg.Storage.Driver != "sqlite" {
				return fmt.Errorf("restore requires sqlite storage")
			}
			if backupPath == "" {
				return fmt.Errorf("restore requires --in")
			}
			result, err := storage.RestoreSQLite(cmd.Context(), backupPath, cfg.Storage.Path)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(out, "restored=%s pre_restore=%s checksum=%s\n", result.TargetPath, result.PreRestorePath, result.BackupChecksum)
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	cmd.Flags().StringVar(&backupPath, "in", "", "backup input path")
	return cmd
}

func newDataCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "data",
		Short: "Manage persisted agent data",
	}
	cmd.AddCommand(newDataRetentionCommand(out))
	cmd.AddCommand(newDataExportCommand(out))
	return cmd
}

func newDataRetentionCommand(out io.Writer) *cobra.Command {
	var configPath string
	var auditDays int
	var runtimeDays int
	var qualityDays int
	var apply bool
	cmd := &cobra.Command{
		Use:   "retention",
		Short: "Plan or apply SQLite data retention",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Context(), config.Options{Path: configPath})
			if err != nil {
				return err
			}
			if cfg.Storage.Driver != "sqlite" {
				return fmt.Errorf("data retention requires sqlite storage")
			}
			repo, err := storage.OpenSQLite(cmd.Context(), cfg.Storage.Path)
			if err != nil {
				return err
			}
			defer func() {
				closeRepositoryLogging(slog.Default(), repo, "retention")
			}()
			policy := storage.RetentionPolicy{
				Now:                  time.Now(),
				AuditMaxAge:          daysDuration(auditDays),
				RuntimeEventMaxAge:   daysDuration(runtimeDays),
				QualityHistoryMaxAge: daysDuration(qualityDays),
				DryRun:               !apply,
			}
			var plan storage.RetentionPlan
			if apply {
				plan, err = repo.ApplyRetention(cmd.Context(), policy)
			} else {
				plan, err = repo.PlanRetention(cmd.Context(), policy)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(
				out,
				"retention dry_run=%t audit_events_deleted=%d runtime_events_deleted=%d quality_history_deleted=%d\n",
				plan.DryRun,
				plan.AuditEventsDeleted,
				plan.RuntimeEventsDeleted,
				plan.QualityHistoryDeleted,
			)
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	cmd.Flags().IntVar(&auditDays, "audit-days", 0, "delete audit events older than this many days")
	cmd.Flags().IntVar(&runtimeDays, "runtime-days", 0, "delete runtime events older than this many days")
	cmd.Flags().IntVar(&qualityDays, "quality-days", 0, "delete quality history older than this many days")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the retention plan instead of dry-running it")
	return cmd
}

func daysDuration(days int) time.Duration {
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

type dataExportSnapshot struct {
	AuditEvents   []domain.AuditEvent   `json:"audit_events"`
	RuntimeEvents []domain.RuntimeEvent `json:"runtime_events"`
}

func newDataExportCommand(out io.Writer) *cobra.Command {
	var configPath string
	var exportPath string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export a SQLite data snapshot",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Context(), config.Options{Path: configPath})
			if err != nil {
				return err
			}
			if cfg.Storage.Driver != "sqlite" {
				return fmt.Errorf("data export requires sqlite storage")
			}
			if exportPath == "" {
				return fmt.Errorf("data export requires --out")
			}
			repo, err := storage.OpenSQLite(cmd.Context(), cfg.Storage.Path)
			if err != nil {
				return err
			}
			defer func() {
				closeRepositoryLogging(slog.Default(), repo, "export")
			}()
			audits, err := repo.ListAuditEvents(cmd.Context())
			if err != nil {
				return err
			}
			events, err := repo.ListRuntimeEvents(cmd.Context())
			if err != nil {
				return err
			}
			body, err := json.MarshalIndent(dataExportSnapshot{
				AuditEvents:   audits,
				RuntimeEvents: events,
			}, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal data export: %w", err)
			}
			if err := os.WriteFile(exportPath, append(body, '\n'), 0o600); err != nil {
				return fmt.Errorf("write data export %q: %w", exportPath, err)
			}
			_, err = fmt.Fprintf(out, "export=%s audit_events=%d runtime_events=%d\n", exportPath, len(audits), len(events))
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	cmd.Flags().StringVar(&exportPath, "out", "", "export output path")
	return cmd
}

func newSkillCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage agent skills",
	}
	cmd.AddCommand(newSkillSyncCommand(out))
	return cmd
}

func newSkillSyncCommand(out io.Writer) *cobra.Command {
	var configPath string
	var registryURL string
	var installRoot string
	var agentName string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync skills from a registry index",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Context(), config.Options{Path: configPath})
			if err != nil {
				return err
			}
			if agentName != "" {
				agentCfg, ok, err := loadSkillCommandAgentConfig(cmd.Context(), cfg, configPath, agentName)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("agent %q not found in config agents", agentName)
				}
				if registryURL == "" {
					registryURL = firstNonEmpty(agentCfg.Skills.RegistryURL, cfg.Skills.RegistryURL)
				}
				if installRoot == "" {
					installRoot = firstNonEmpty(agentCfg.Skills.InstallRoot, cfg.Skills.InstallRoot)
				}
			}
			if registryURL == "" {
				registryURL = cfg.Skills.RegistryURL
			}
			if installRoot == "" {
				installRoot = cfg.Skills.InstallRoot
			}
			if registryURL == "" {
				return fmt.Errorf("skill sync requires --registry-url or skills.registry_url")
			}
			if installRoot == "" {
				return fmt.Errorf("skill sync requires --install-root or skills.install_root")
			}
			repository := skill.Repository(skill.NewMemoryRepository())
			var audit port.AuditLog
			var closeRepository func()
			if cfg.Storage.Driver == "sqlite" {
				repo, err := storage.OpenSQLite(cmd.Context(), cfg.Storage.Path)
				if err != nil {
					return err
				}
				repository = repo
				audit = storage.NewSQLiteAuditLog(repo)
				closeRepository = func() {
					closeRepositoryLogging(slog.Default(), repo, "skill-sync")
				}
			} else {
				closeRepository = func() {}
			}
			defer closeRepository()
			syncer := skill.NewRegistrySyncer(skill.RegistrySyncConfig{
				IndexURL:    registryURL,
				InstallRoot: installRoot,
				Repository:  repository,
				Scanner:     skill.NewSecurityScanner(),
				Audit:       audit,
			})
			report, err := syncer.Sync(cmd.Context())
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "skill_sync installed=%d quarantined=%d failed=%d\n", report.Installed, report.Quarantined, report.Failed); err != nil {
				return err
			}
			// A bare failed=N tells the operator nothing actionable: not which
			// skill, not whether it was the network, a malformed manifest or a
			// hostile package. Print what the report now carries.
			for _, failure := range report.Failures {
				if _, err := fmt.Fprintf(out, "  failed %s: %s\n", failure.ManifestURL, failure.Reason); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	cmd.Flags().StringVar(&registryURL, "registry-url", "", "skill registry index URL")
	cmd.Flags().StringVar(&installRoot, "install-root", "", "skill install root")
	cmd.Flags().StringVar(&agentName, "agent", "", "registered agent id whose skills config should be used")
	return cmd
}

func loadSkillCommandAgentConfig(ctx context.Context, cfg config.Config, configPath string, agentName string) (agentregistry.AgentConfig, bool, error) {
	configDir := "."
	if configPath != "" {
		configDir = filepath.Dir(configPath)
	}
	registry, err := agentregistry.Load(ctx, cfg, configDir)
	if err != nil {
		return agentregistry.AgentConfig{}, false, err
	}
	agentCfg, ok := registry.Get(agentName)
	return agentCfg, ok, nil
}

func newVersionCommand(out io.Writer) *cobra.Command {
	var plain bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print Legion Agent version information",
		RunE: func(*cobra.Command, []string) error {
			info := version.Info()
			if plain {
				_, err := fmt.Fprintf(out, "version=%s commit=%s build_time=%s\n", info.Version, info.Commit, info.BuildTime)
				return err
			}
			_, err := fmt.Fprintf(out, "Legion Agent %s\ncommit: %s\nbuild_time: %s\n", info.Version, info.Commit, info.BuildTime)
			return err
		},
	}
	cmd.Flags().BoolVar(&plain, "plain", false, "print machine-readable version output")
	return cmd
}

func newDoctorCommand(out io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check local agent setup",
		RunE: func(*cobra.Command, []string) error {
			_, err := fmt.Fprintln(out, "agent setup ok")
			return err
		},
	}
}

// ServeOptions holds the parameters needed to build a serve Service.
// Extracted so legionAgentGUI can call this without the cobra command wrapper.
type ServeOptions struct {
	ConfigPath string
	Addr       string
	Logger     *slog.Logger
}

// ServeResult holds the running service and a cleanup function.
type ServeResult struct {
	Service  *service.Service
	Listener net.Listener
	Close    func()
}

// defaultTaskRunner is the agentruntime.TaskRunner the coordinator dispatches
// a task to when no per-agent resolver runner is resolved for it (an empty
// task.AgentID, or an AgentID missing from the registry — see
// AgentRuntimeResolver.ResolveTaskRunner). Before Task 7 (M3a) this path used
// a single *agentruntime.Runtime whose tool registry (and thus its
// WorkspacePathGuard sandbox root) was built once, at serve assembly time,
// rooted at cfg.ContextFiles.Root. That pinned every default-agent task to
// one fixed sandbox regardless of task.WorkingDir, unlike the per-agent path
// (agentToolRoot in the runtime package), which already re-resolves the
// sandbox root on every ResolveTaskRunner call.
//
// defaultTaskRunner closes that gap: RunTask rebuilds the tool registry fresh
// on every call, rooted at task.WorkingDir when the task carries one
// (falling back to contextRoot otherwise) — the same task.WorkingDir-first
// priority as agentToolRoot. Every other runtime setting (model, audit,
// events, cognitive context builder, tool-round budget, checkpoints, tool
// gate) is fixed at serve assembly and reused unchanged across calls via
// runtimeCfg; only Tools is overwritten per call.
type defaultTaskRunner struct {
	// runtimeCfg is the template runtime configuration shared by every call;
	// its Tools field is always overwritten with the freshly built per-task
	// registry before constructing the runtime for that call.
	runtimeCfg agentruntime.Config
	// contextRoot is the sandbox root used when a task has no WorkingDir
	// (cfg.ContextFiles.Root at serve assembly).
	contextRoot     string
	audit           port.AuditLog
	taskLedger      *taskledger.Ledger
	messageStore    tool.AgentMessageStore
	sessionSearcher tool.MessageSearcher
	webOptions      tool.WebToolOptions
	maasResolver    agentruntime.ModelResolver
}

// RunTask builds a fresh read-only workspace tool registry rooted at
// task.WorkingDir (or contextRoot, when the task has none), registers on it
// every tool the pre-M3a fixed default registry carried — task ledger, agent
// messaging, web, session search, MoA consult — plus delegate_task (the
// default runtime is always a root orchestrator), then runs the task on a
// freshly constructed *agentruntime.Runtime built from runtimeCfg with that
// registry. Constructing a fresh Runtime per call is cheap (a struct literal;
// no I/O), so this trades a small per-call allocation for a per-task sandbox
// root — an intentional trade given the tool sandbox is a security boundary
// (CLAUDE.md fail-loud/security posture).
func (d *defaultTaskRunner) RunTask(ctx context.Context, agent domain.Agent, task domain.Task) (domain.TaskRun, error) {
	root := strings.TrimSpace(task.WorkingDir)
	if root == "" {
		root = d.contextRoot
	}
	tools := tool.NewFileReadOnlyWorkspaceRegistry(root, d.audit)
	tool.RegisterTaskLedgerTools(tools, d.taskLedger)
	tool.RegisterAgentMessageTools(tools, d.messageStore)
	tool.RegisterWebTools(tools, d.webOptions)
	tool.RegisterSessionSearchTool(tools, d.sessionSearcher)
	agentruntime.RegisterMoAConsultTool(tools, d.maasResolver)
	runtimeCfg := d.runtimeCfg
	runtimeCfg.Tools = tools
	rt := agentruntime.NewRuntime(runtimeCfg)
	rt.RegisterDelegateTaskTool(tools)
	return rt.RunTask(ctx, agent, task)
}

// webToolOptions maps the web config block onto the tool package options.
func webToolOptions(cfg config.WebToolConfig) tool.WebToolOptions {
	return tool.WebToolOptions{
		Enabled:           cfg.Enabled,
		AllowPrivateHosts: cfg.AllowPrivateHosts,
		Timeout:           time.Duration(cfg.TimeoutSeconds) * time.Second,
		MaxBytes:          int64(cfg.MaxResponseKB) * 1024,
		Allowlist:         cfg.Allowlist,
	}
}

// skillsRootAvailable reports whether the configured skills install root exists
// as a directory. The skill loader walks this root and fails loud when it is
// missing, so the serve path treats an absent root as "no skills installed"
// (a valid optional deployment) and skips skill injection rather than failing
// every task's context build. A path that exists but is not a directory is a
// misconfiguration and is simply not treated as available here.
// skillsRootAvailable delegates to skill.RootAvailable so the default runtime
// and the per-agent runtimes gate their skills mount on the exact same rule.
// They used to disagree — the resolver checked only for a non-empty path — and
// every task routed to a sub-agent whose install_root did not exist yet failed.
func skillsRootAvailable(root string) bool {
	return skill.RootAvailable(root)
}

// serveEventBusBuffer is the per-subscriber channel buffer for the platform
// event bus backing /v1/events. Large enough that a normally-paced SSE client
// keeps up; a stalled client still drops rather than blocking publishers.
const serveEventBusBuffer = 256

// SessionLister enumerates persisted agent sessions so distinctSessionBases
// can discover every working_dir a session has ever been bound to.
// server.SessionStore satisfies it: ListAgentSessions with an empty
// companyID/agentID lists every session (SQLiteRepository treats an empty
// filter argument as "match all", not "match none"). A nil lister is a valid
// "non-persistent storage.Driver" deployment (serviceStores returns a nil
// server.SessionStore in that mode) — there is no session history to
// enumerate working_dir bases from, not an error.
type SessionLister interface {
	ListAgentSessions(ctx context.Context, companyID, agentID string) ([]domain.AgentSession, error)
}

// distinctSessionBases returns the deduplicated set of session-state bases
// currently in use: workspaceRoot (the default base for a session with no
// working_dir) union sessionstate.SessionBase(workspaceRoot, s.WorkingDir) for
// every persisted session's working_dir. Restart recovery and the approval
// timeout sweep (BuildServeService below) scan every base this returns, so a
// suspended checkpoint or pending approval ticket filed under a
// working_dir-bound session is not silently missed just because it is not
// under the workspace root. Fail-loud: an error from
// sessions.ListAgentSessions aborts rather than returning a partial,
// silently-incomplete base set.
func distinctSessionBases(ctx context.Context, sessions SessionLister, workspaceRoot string) ([]string, error) {
	bases := []string{workspaceRoot}
	if sessions == nil {
		return bases, nil
	}
	all, err := sessions.ListAgentSessions(ctx, "", "")
	if err != nil {
		return nil, fmt.Errorf("list agent sessions for base enumeration: %w", err)
	}
	seen := map[string]struct{}{workspaceRoot: {}}
	for _, s := range all {
		base := sessionstate.SessionBase(workspaceRoot, s.WorkingDir)
		if _, ok := seen[base]; ok {
			continue
		}
		seen[base] = struct{}{}
		bases = append(bases, base)
	}
	return bases, nil
}

// BuildServeService constructs and returns a ready-to-Start service from the
// same dependency wiring as newServeCommand, but without cobra.
func BuildServeService(ctx context.Context, opts ServeOptions) (ServeResult, error) {
	cfg, err := config.Load(ctx, config.Options{Path: opts.ConfigPath})
	if err != nil {
		return ServeResult{}, err
	}
	addr := opts.Addr
	if addr == "" {
		addr = cfg.Server.ListenAddr
	}
	if addr == "" {
		addr = "127.0.0.1:0" // random port for GUI mode
	}

	taskStore, workflowStore, sessionStore, readiness, closeStore, err := serviceStores(ctx, cfg)
	if err != nil {
		return ServeResult{}, err
	}

	var auditLog port.AuditLog
	var qualityEvals server.QualityEvalStore
	var messageStore tool.AgentMessageStore
	var sessionSearcher tool.MessageSearcher
	var skillCurator *skill.Curator
	// taskSink is the durable store the live scheduler writes state changes
	// through to. It stays nil for the non-sqlite drivers, whose "store" is the
	// in-memory scheduler itself and has nothing to persist to.
	var taskSink task.TaskSink
	// skillUsage is the shared usage sidecar: the skill System records activity on
	// it as skills are selected into task context, and the Curator sweep reads it
	// to age idle skills. Sharing one instance connects the two.
	skillUsage := skill.NewUsageStore()
	if repo, ok := taskStore.(*storage.SQLiteRepository); ok {
		auditLog = storage.NewSQLiteAuditLog(repo)
		qualityEvals = repo
		messageStore = repo
		sessionSearcher = repo
		taskSink = repo
		curator, err := skill.NewCurator(skill.CuratorConfig{Repository: repo, Usage: skillUsage})
		if err != nil {
			closeStore()
			return ServeResult{}, err
		}
		skillCurator = curator
	}
	if auditLog == nil {
		auditLog = adapter.NewMemoryAuditLog()
	}

	logger := opts.Logger
	if logger == nil {
		fileLogger, err := defaultLogger()
		if err != nil {
			closeStore()
			return ServeResult{}, err
		}
		logger = fileLogger
	}

	// platformEvents backs the /v1/events SSE stream (push/subscribe). The
	// bridge tees every RuntimeEvent the runtime/coordinator/workflow engine
	// already publishes into it, so lifecycle events reach SSE with zero changes
	// to any publisher. Buffer sized generously: a slow SSE subscriber drops
	// events (at-most-once, design §3.4.2), and the /v1/approvals list endpoint
	// (Task 5) is the reconcile path for missed approval prompts.
	platformEvents := observability.NewEventBus(serveEventBusBuffer)
	workflowEvents := eventbridge.New(platformEvents, logger)
	// Startup invariant: a durable driver whose store never became a sink would
	// silently stop recording task state changes -- the exact defect this wiring
	// exists to prevent, and one with no symptom until someone reads the
	// database. Adding a persistent driver without teaching the branch above
	// about it must fail here rather than at 3am.
	//
	// It also catches a misspelled driver ("sqlite3", "sqllite"), which
	// serviceStores would otherwise quietly serve from memory while the operator
	// believes their data is being written to disk.
	//
	// "" is the unset driver and means the same as "memory" to serviceStores,
	// which selects on the driver being "sqlite"; treating it as durable here
	// would reject a configuration that runs fine.
	if cfg.Storage.Driver != "" && cfg.Storage.Driver != "memory" && taskSink == nil {
		closeStore()
		return ServeResult{}, fmt.Errorf("storage driver %q provides no task sink: task state changes would never be persisted", cfg.Storage.Driver)
	}
	liveTasks := task.NewSchedulerWithSink(taskSink)
	httpTasks := server.TaskStore(liveTasks)
	if taskStore != nil {
		httpTasks = teeTaskStore{live: liveTasks, persistent: taskStore}
	}
	approvals := approval.NewService()
	workflowEngine := workflow.NewEngine(workflow.Config{
		Scheduler: liveTasks,
		Approvals: approvals,
		Events:    workflowEvents,
		Audit:     auditLog,
	})
	registry, err := loadServeAgentRegistry(ctx, cfg, opts.ConfigPath)
	if err != nil {
		closeStore()
		return ServeResult{}, err
	}
	taskLedger, err := newCommandTaskLedger(cfg)
	if err != nil {
		closeStore()
		return ServeResult{}, err
	}
	// Manual-mode approval gate wiring (M2b). A single ToolGateStore persists
	// approval tickets under the workspace root (the same base checkpointStore
	// uses), and one ManualToolGate instance is shared by the default runtime
	// and every resolver-built per-agent runtime, so Manual-mode suspend/resume
	// behaves identically regardless of which runtime dispatches the tool call.
	workspaceRoot, workspaceRootWarning := sessionstate.ResolveWorkspaceRoot(cfg.Workspace.Root)
	checkpointStore := sessionstate.NewStore(workspaceRoot)
	toolGateStore := approval.NewToolGateStore(workspaceRoot)
	// approvalSink translates ShouldSuspend/Decide notifications into
	// approval_pending/approval_resolved envelopes on platformEvents (the
	// /v1/events SSE stream). It is best-effort and error-less by contract: the
	// on-disk ticket in toolGateStore is the source of truth.
	approvalSink := newPlatformApprovalSink(platformEvents, logger)
	manualGate := manualgate.New(toolGateStore, manualgate.WithApprovalSink(approvalSink))
	// approvalCoordinator applies a human's approve/deny decision (HTTP handler
	// below) and, once every ticket for a task is decided, flips the task
	// Suspended->Running so the coordinator's resume scan re-dispatches it. It
	// also drives the background timeout sweep and the restart reconcile below.
	approvalCoordinator := manualgate.NewApprovalCoordinator(toolGateStore, liveTasks, manualgate.WithCoordinatorSink(approvalSink))
	resolver := agentruntime.NewAgentRuntimeResolver(agentruntime.AgentRuntimeResolverConfig{
		Registry:     registry,
		RootConfig:   cfg,
		Audit:        auditLog,
		Events:       workflowEvents,
		TaskLedger:   taskLedger,
		MessageStore: messageStore,
		MaasFactory:  maasFactoryFromConfig(cfg.Maas),
		Checkpoints:  checkpointStore,
		ToolGate:     manualGate,
		Logger:       logger,
	})
	defaultMaas, err := adapter.NewMaasClientFromProfile(cfg.Maas, "")
	if err != nil {
		closeStore()
		return ServeResult{}, err
	}
	if defaultMaas == nil {
		defaultMaas = adapter.NewRecordingMaas(cfg.Runtime.DemoResponse)
	}
	defaultDisplay := tuiDisplayConfig(cfg.Maas, "", "")
	defaultContext, err := buildRunContextPrefix(ctx, cfg, false, defaultDisplay.ModelName)
	if err != nil {
		closeStore()
		return ServeResult{}, err
	}
	// Cognitive evolution wiring (L4 memory / L5 learning). The capability
	// memory store is shared: the GEP cycle (L5) solidifies learned genes into
	// it, and the cognitive Core (L4) reads them back when building context, so
	// failures distilled by the background scan resurface as capability hints.
	capabilityStore := memory.NewCapabilityMemoryStore()
	episodicMemory := newEpisodicMemoryProvider(memory.NewEpisodicMemoryStore(adapter.KeywordEmbeddingProvider{}), 3)
	gepCycle := evolution.NewGepCycle(evolution.GepCycleConfig{
		Extractor:       evolution.NewSignalExtractor(),
		Distiller:       evolution.DefaultDistillationOperator{},
		CapabilityStore: capabilityStore,
		EventLog:        evolution.NewEvolutionEventLog(auditLog),
	})
	// L6 trust scoring: observe trust-relevant runtime learning events and feed
	// them into the trust score manager so per-agent scores stay queryable. This
	// is a minimal, non-invasive integration (event subscription only) and does
	// not yet gate dispatch in the coordinator.
	trustManager := quality.NewTrustScoreManager()

	// Cognitive Core for the default runtime: L4 memory + capability recall on
	// top of the compressor and context files. Skills (L1 injection) are mounted
	// only when an install root is actually present; an absent root is a valid
	// "no skills installed" deployment, not an error, and must not fail context
	// building for every task (skill.System.Load fails loud on a missing root).
	defaultCore := cognitive.NewCore(cognitive.NewContextCompressor(cognitive.DefaultContextCompressorConfig(defaultMaas))).
		WithContextFiles(defaultContext).
		WithMemory(episodicMemory).
		WithCapabilityMemory(capabilityStore)
	// capabilitySkills is the skill half of the default runtime's capability
	// catalog. It is set only when a skills root is present; the runtime builds
	// the tool half per task from the per-task registry. WithSkills is retained
	// for the /skills query paths but no longer injects into the prompt.
	var capabilitySkills capability.Provider
	if skillsRootAvailable(cfg.Skills.InstallRoot) {
		defaultSkillSystem := skill.NewSystem(skill.Config{
			Roots:   []string{cfg.Skills.InstallRoot},
			Scanner: skill.NewSecurityScanner(),
		}).WithUsage(skillUsage, time.Now)
		defaultCore = defaultCore.WithSkills(defaultSkillSystem)
		capabilitySkills = capability.NewSkillProvider(defaultSkillSystem)
	}

	// The default task runner rebuilds its tool registry (and thus the
	// WorkspacePathGuard sandbox root) on every RunTask call, rooted at
	// task.WorkingDir when the task carries one (falling back to
	// cfg.ContextFiles.Root otherwise) — see defaultTaskRunner's doc comment
	// for why (Task 7, M3a). Everything else (model, audit, events, cognitive
	// context builder, checkpoints, tool gate) is fixed at serve assembly and
	// shared across every call, same as before.
	defaultRunner := &defaultTaskRunner{
		runtimeCfg: agentruntime.Config{
			Maas:             defaultMaas,
			Audit:            auditLog,
			Events:           workflowEvents,
			ContextBuilder:   defaultCore,
			MaxToolRounds:    cfg.Runtime.MaxToolRounds,
			LazyTools:        cfg.Runtime.LazyTools,
			Checkpoints:      checkpointStore,
			ToolGate:         manualGate,
			Logger:           logger,
			CapabilitySkills: capabilitySkills,
		},
		contextRoot:     cfg.ContextFiles.Root,
		audit:           auditLog,
		taskLedger:      taskLedger,
		messageStore:    messageStore,
		sessionSearcher: sessionSearcher,
		webOptions:      webToolOptions(cfg.Web),
		maasResolver:    maasProfileResolver{cfg: cfg.Maas},
	}
	coordinator := agentruntime.NewCoordinator(agentruntime.CoordinatorConfig{
		Agent: domain.Agent{
			ID:        "default-agent",
			CompanyID: "default-company",
			Role:      "developer",
			Status:    domain.AgentActive,
		},
		Scheduler:          liveTasks,
		Locks:              task.NewLockStore(),
		Runtime:            defaultRunner,
		TaskRunnerResolver: resolver,
		Reviewer:           quality.NewAegisReviewer(),
		Evaluator:          quality.NewEvalEngine(3),
		Approvals:          approvals,
		Audit:              auditLog,
		Events:             workflowEvents,
		TrustGate:          trustManager,
		MaxWorkers:         cfg.Runtime.MaxConcurrentTasks,
		Checkpoints:        checkpointStore,
		Logger:             logger,
	})
	background := task.NewBackgroundScheduler()
	background.AddJob("agent-coordinator-heartbeat", func(ctx context.Context) error {
		_, _, err := coordinator.Heartbeat(ctx)
		return err
	})
	// L5 learning loop: scan runtime learning events (task/tool failures, hard
	// loops, budget exhaustion) published by the runtime and coordinator onto
	// workflowEvents, and drive the GEP cycle to distill capability genes.
	background.AddJob("gep-failure-scan", task.NewGepFailureScanJob(workflowEvents, gepCycle))
	// L6 trust scoring loop: translate trust-relevant learning events into
	// security events for the trust score manager.
	background.AddJob("trust-score-scan", newTrustScoreScanJob(workflowEvents, trustManager))
	// Skill lifecycle: deterministic, zero-token Curator sweep that ages idle
	// workspace skills through stale into archived (never deletes). No-op when no
	// persistent skill repository is configured.
	if skillCurator != nil {
		background.AddJob("skill-curator-sweep", newSkillCuratorSweepJob(skillCurator, time.Now))
	}
	// Reinject completed background sub-tasks: a subtask_completed event becomes a
	// result AgentMessage on the parent task, which the parent agent reads on its
	// next round. Requires a message store (persistent deployment).
	if messageStore != nil {
		background.AddJob("subtask-reinjection", newSubtaskReinjectionJob(workflowEvents, messageStore))
	}
	// L6 degradation detection: periodically evaluate per-agent task-quality
	// trends and publish a degradation alert when quality drops past the
	// configured threshold across the evaluation window.
	degradationEvaluator := quality.NewEvolutionEvaluator(quality.EvolutionEvaluatorConfig{
		EventBus:             workflowEvents,
		QualityDropThreshold: cfg.Evolution.DegradationThreshold,
		Window:               time.Duration(cfg.Evolution.DegradationWindowDays) * 24 * time.Hour,
	})
	background.AddJob("degradation-scan", newDegradationScanJob(
		workflowEvents,
		degradationEvaluator,
		time.Duration(cfg.Evolution.DegradationScanMinutes)*time.Minute,
		nil,
	))
	metrics := observability.NewMetricsRecorder(nil)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		closeStore()
		return ServeResult{}, fmt.Errorf("listen on %q: %w", addr, err)
	}

	if workspaceRootWarning != "" {
		logger.Warn("workspace root fallback", "detail", workspaceRootWarning)
	}
	// Restart recovery must scan every session-state base in use, not just the
	// workspace root: a task suspended under a working_dir-bound session (M3)
	// files its checkpoint under SessionBase(workspaceRoot, workingDir), which
	// checkpointStore.ListSuspended (workspace-root-only) never sees.
	recoveryBases, err := distinctSessionBases(ctx, sessionStore, workspaceRoot)
	if err != nil {
		_ = listener.Close()
		closeStore()
		return ServeResult{}, fmt.Errorf("enumerate session bases for restart recovery: %w", err)
	}
	var suspendedCheckpoints []sessionstate.Checkpoint
	for _, base := range recoveryBases {
		cps, err := checkpointStore.ListSuspendedIn(base)
		if err != nil {
			_ = listener.Close()
			closeStore()
			return ServeResult{}, fmt.Errorf("list suspended checkpoints in base %q: %w", base, err)
		}
		suspendedCheckpoints = append(suspendedCheckpoints, cps...)
	}
	recovered, err := coordinator.RecoverSuspended(ctx, suspendedCheckpoints)
	if err != nil {
		_ = listener.Close()
		closeStore()
		return ServeResult{}, fmt.Errorf("recover suspended tasks: %w", err)
	}
	if recovered > 0 {
		logger.Info("recovered suspended tasks", "count", recovered)
	}
	// Restart reconcile: a task suspended awaiting Manual-mode tool approval
	// whose ticket(s) were all decided before the process died (approved, but
	// the resume dispatch never ran) must resume now rather than wait for a
	// fresh decision that will never come. ReconcileResume leaves untouched a
	// Suspended task with no tickets (suspended for another reason) or with
	// any ticket still pending (still waiting on a human).
	suspendCandidates, err := liveTasks.List(ctx)
	if err != nil {
		_ = listener.Close()
		closeStore()
		return ServeResult{}, fmt.Errorf("list tasks for approval reconcile: %w", err)
	}
	for _, st := range suspendCandidates {
		if st.Status != domain.TaskSuspended {
			continue
		}
		if err := approvalCoordinator.ReconcileResume(ctx, st.ID); err != nil {
			_ = listener.Close()
			closeStore()
			return ServeResult{}, fmt.Errorf("reconcile approval resume for task %s: %w", st.ID, err)
		}
	}
	// Surface background tick failures (e.g. a task failing in the coordinator
	// heartbeat) instead of dropping them silently.
	background.SetLogger(logger)
	// Manual-mode approval timeout sweep: auto-deny any ApprovalPending ticket
	// older than the configured TTL so a human's silence does not wedge a task
	// forever. Registered here (not with the other AddJob calls above) because
	// NewTimeoutSweepJob needs logger, which is not constructed until this
	// point in serve assembly.
	approvalTTL := time.Duration(cfg.Runtime.ApprovalTimeoutSeconds) * time.Second
	if cfg.Runtime.ApprovalTimeoutSeconds <= 0 {
		// Documented contract default (config.RuntimeConfig.ApprovalTimeoutSeconds
		// doc comment), not a silent fallback: an explicit 0/negative value in a
		// loaded config still means "use the 5-minute default" — the same default
		// defaultConfig() sets for an omitted field.
		approvalTTL = 300 * time.Second
	}
	// basesFn re-enumerates session bases on every sweep tick (not once at
	// startup) so a session bound to a new working_dir after serve starts is
	// still covered by the next sweep pass.
	background.AddJob("approval-timeout-sweep", manualgate.NewTimeoutSweepJob(toolGateStore, approvalCoordinator, approvalTTL, time.Now, logger, func(ctx context.Context) ([]string, error) {
		return distinctSessionBases(ctx, sessionStore, workspaceRoot)
	}))

	// Skill management endpoints (/v1/skills/*) back the GUI's /skill commands.
	// The disk manager is constructed whenever an install root is configured;
	// the directory itself may not exist yet (install creates it). When no root
	// is configured the manager stays nil and the endpoints report 503.
	var skillManager server.SkillManager
	if strings.TrimSpace(cfg.Skills.InstallRoot) != "" {
		skillManager = skill.NewDiskManager(cfg.Skills.InstallRoot, skill.NewSecurityScanner())
	}

	httpServer := server.NewHTTPServer(server.Config{
		Tasks:               httpTasks,
		Agents:              registry,
		Workflows:           workflowStore,
		WorkflowEngine:      workflowEngine,
		WorkflowEvents:      workflowEvents,
		PlatformEvents:      platformEvents,
		Readiness:           readiness,
		WorkspaceRoot:       workspaceRoot,
		AdminToken:          cfg.Server.AdminToken,
		PublicHealthEnabled: cfg.Server.PublicHealthEnabled,
		RequireIdentity:     cfg.Server.RequireIdentity,
		RequestIDHeader:     cfg.Server.RequestIDHeader,
		Audit:               auditLog,
		QualityEvals:        qualityEvals,
		Sessions:            sessionStore,
		Messages:            messageStore,
		Skills:              skillManager,
		Logger:              logger,
		Metrics:             metrics,
		ToolApprovals:       approvalCoordinator,
		ApprovalTickets:     toolGateStore,
		Diagnostics: observability.NewDiagnostics(observability.DiagnosticsConfig{
			Version:             "dev",
			StorageDriver:       cfg.Storage.Driver,
			StoragePath:         cfg.Storage.Path,
			MaasBaseURL:         cfg.Maas.BaseURL,
			MaasAPIKey:          cfg.Maas.APIKey,
			AdminToken:          cfg.Server.AdminToken,
			RuntimeDemoResponse: cfg.Runtime.DemoResponse,
			SchedulerEnabled:    true,
			SchedulerRunning:    true,
			Metrics:             metrics,
		}),
	})
	svc, err := service.New(service.ServiceConfig{
		Config:    cfg,
		Scheduler: background,
		HTTPServer: &http.Server{
			Handler: httpServer,
		},
		Listener: listener,
		Logger:   logger,
	})
	if err != nil {
		_ = listener.Close()
		closeStore()
		return ServeResult{}, err
	}
	return ServeResult{
		Service:  svc,
		Listener: listener,
		// Close drains in-flight task goroutines before releasing storage.
		// Service.Start only returns once the background scheduler (and thus
		// task dispatch) has stopped, so by the time Close runs no new tasks
		// can start; coordinator.Wait() blocks for the ones already running
		// to finish, and only then does closeStore() tear down storage, so a
		// task goroutine can never write to an already-closed store.
		Close: func() {
			coordinator.Wait()
			if err := platformEvents.Close(); err != nil {
				logger.Warn("close platform event bus", "error", err)
			}
			closeStore()
		},
	}, nil
}
