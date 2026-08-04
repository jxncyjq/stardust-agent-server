package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/browser"
	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/contextfiles"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/skill"
	"github.com/stardust/legion-agent/internal/taskledger"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

type MaasRunnerFactoryResult struct {
	Client    port.MaasInferenceClient
	ModelName string
}

type MaasRunnerFactory func(profile string) (MaasRunnerFactoryResult, error)

// ConversationTurnLister loads a session's recent conversation turns. It is the
// one method the resolver needs from the session store, kept as its own
// interface so the runtime package does not depend on the whole store.
type ConversationTurnLister interface {
	ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error)
}

type AgentRuntimeResolverConfig struct {
	Registry     *agentregistry.Registry
	RootConfig   config.Config
	Audit        port.AuditLog
	Events       port.EventBus
	TaskLedger   *taskledger.Ledger
	MessageStore tool.AgentMessageStore
	MaasFactory  MaasRunnerFactory
	// Checkpoints persists suspended tool-loop state for resolver-built (per-agent)
	// runtimes, mirroring Config.Checkpoints on the default runtime. Nil disables
	// suspend/resume for those runtimes (legacy behaviour).
	Checkpoints *sessionstate.Store
	// ToolGate gates each tool round for resolver-built runtimes, mirroring
	// Config.ToolGate on the default runtime. Nil never suspends.
	ToolGate ToolGate
	// Logger reports conditions that are tolerated but worth surfacing, such as
	// a configured skills root that does not exist. Nil disables that reporting
	// (tests, embedded use); it never changes what the resolver builds.
	Logger *slog.Logger
	// SkillUsage records that a skill was actually loaded, mirroring
	// Config.SkillUsage on the default runtime. It is the same shared
	// *skill.UsageStore instance the Curator sweep reads (see command.go), so a
	// skill loaded by any per-agent runtime ages the same as one loaded by the
	// default runtime. Nil disables aging for every resolver-built runtime
	// (skill.Curator "no usage history" — never touched, never swept).
	SkillUsage SkillUsageRecorder
	// ConversationTurns loads the session history injected into each task's
	// prompt (the "Recent conversation" block). Nil disables injection — the
	// serve path without a session store, and tests. Without it a GUI task runs
	// with no cross-turn memory at all.
	ConversationTurns ConversationTurnLister
	// EpisodeRecorder distills each resolver-built runtime's finished tasks
	// into the episodic store, mirroring Config.EpisodeRecorder on the default
	// runtime. Nil disables episodic recording for those runtimes.
	EpisodeRecorder EpisodeRecorder
}

type AgentRuntimeResolver struct {
	registry          *agentregistry.Registry
	rootConfig        config.Config
	audit             port.AuditLog
	events            port.EventBus
	taskLedger        *taskledger.Ledger
	messageStore      tool.AgentMessageStore
	maasFactory       MaasRunnerFactory
	checkpoints       *sessionstate.Store
	toolGate          ToolGate
	logger            *slog.Logger
	skillUsage        SkillUsageRecorder
	conversationTurns ConversationTurnLister
	episodeRecorder   EpisodeRecorder
}

func NewAgentRuntimeResolver(cfg AgentRuntimeResolverConfig) *AgentRuntimeResolver {
	return &AgentRuntimeResolver{
		registry:          cfg.Registry,
		rootConfig:        cfg.RootConfig,
		audit:             cfg.Audit,
		events:            cfg.Events,
		taskLedger:        cfg.TaskLedger,
		messageStore:      cfg.MessageStore,
		maasFactory:       cfg.MaasFactory,
		checkpoints:       cfg.Checkpoints,
		toolGate:          cfg.ToolGate,
		logger:            cfg.Logger,
		skillUsage:        cfg.SkillUsage,
		conversationTurns: cfg.ConversationTurns,
		episodeRecorder:   cfg.EpisodeRecorder,
	}
}

// resolveHomeDir returns the user's home directory, used to exclude the
// resident ~/.stardust/agents.md from on-demand subtree injection
// (tool.WithAgentsInjection's homeDir). Injection is a context-quality
// nicety, not something a per-agent task should fail over for, so a
// resolution failure degrades to "" (only the two workspace-local resident
// paths are excluded) rather than failing ResolveTaskRunner — but the miss is
// logged (Warn), not silently dropped, per CLAUDE.md fail-loud.
func (r *AgentRuntimeResolver) resolveHomeDir(ctx context.Context) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		if r.logger != nil {
			r.logger.WarnContext(ctx, "resolve home directory",
				"component", "agent_resolver",
				"consequence", "resident global agents.md will not be excluded from subtree injection",
				"error", err)
		}
		return ""
	}
	return homeDir
}

// recentTurnsForTask loads the session turns to inject into this task's prompt:
// the most recent DefaultRecentTurns turns of task.SessionID, excluding the
// task's own user turn (the HTTP layer records it before enqueuing, so it would
// otherwise be duplicated alongside task.Input), each truncated to
// Session.MaxTurnChars.
//
// A nil lister or an empty task.SessionID is a legitimate "no session history"
// state and yields (nil, nil). A store failure is NOT: it returns an error, so
// a lost history is never mistaken for an empty one (CLAUDE.md fail-loud).
func (r *AgentRuntimeResolver) recentTurnsForTask(ctx context.Context, task domain.Task) ([]domain.ConversationTurn, error) {
	// Delegated to the shared helper so this path and the CLI's
	// defaultTaskRunner cannot drift apart — wiring history into only one of
	// them is exactly how the GUI ended up with no cross-turn memory.
	return RecentTurnsForTask(ctx, r.conversationTurns, r.rootConfig.Session, task)
}

func (r *AgentRuntimeResolver) ResolveTaskRunner(ctx context.Context, task domain.Task) (domain.Agent, TaskRunner, bool, error) {
	if r == nil || r.registry == nil || task.AgentID == "" {
		return domain.Agent{}, nil, false, nil
	}
	agentCfg, ok := r.registry.Get(task.AgentID)
	if !ok {
		return domain.Agent{}, nil, false, nil
	}
	// Fail-loud assembly-time validation (CLAUDE.md §0): a disabled_tools entry
	// that does not name a known gateable tool is a config error, not an inert
	// no-op — a typo would otherwise silently disable nothing. gateableNames is
	// fetched once, outside the loop, so validating N entries costs one map
	// build rather than N.
	gateableNames := toolauth.GateableToolNames()
	for _, name := range agentCfg.DisabledTools {
		if !gateableNames[name] {
			return domain.Agent{}, nil, false, fmt.Errorf(
				"agent %q disabled_tools names unknown tool %q (gateable: %v)",
				agentCfg.ID, name, sortedKeys(gateableNames))
		}
	}
	if r.maasFactory == nil {
		return domain.Agent{}, nil, false, fmt.Errorf("maas runner factory is nil")
	}
	maas, err := r.maasFactory(agentCfg.MaasProfile)
	if err != nil {
		return domain.Agent{}, nil, false, fmt.Errorf("create maas runner for profile %q: %w", agentCfg.MaasProfile, err)
	}
	contextBlock, err := loadAgentContextFiles(ctx, r.rootConfig, agentCfg.ContextFiles, agentToolRoot(r.rootConfig, agentCfg, task))
	if err != nil {
		return domain.Agent{}, nil, false, fmt.Errorf("load agent context files for %q: %w", task.AgentID, err)
	}
	contextBuilder := cognitive.NewCore(cognitive.NoopCompressor{}).WithContextFiles(contextBlock)
	// capabilitySkills is the skill half of the capability catalog for this
	// agent. It is set only when a skills root is actually available; the tool
	// half is built per task by the runtime from the effective registry.
	var capabilitySkills capability.Provider
	// skill.RootAvailable, not a bare non-empty check: an install_root that has
	// not been created yet means "no skills installed", and mounting it would
	// fail the skill walk and with it every task routed to this agent. The
	// default runtime gates its own mount the same way.
	if skillsRoot := agentSkillsRoot(r.rootConfig, agentCfg); skillsRoot != "" {
		if skill.RootAvailable(skillsRoot) {
			skillSystem := skill.NewSystem(skill.Config{
				Roots:   []string{skillsRoot},
				Scanner: skill.NewSecurityScanner(),
			})
			// WithSkills is retained for the /skills query paths; it no longer
			// injects into the prompt. Skills reach the model through the capability
			// catalog, whose skill half is this same skill system.
			contextBuilder = contextBuilder.WithSkills(skillSystem)
			capabilitySkills = capability.NewSkillProvider(skillSystem)
		} else if r.logger != nil {
			// Skipping is the right call, but not silently: a configured root
			// that is unusable is far more often a typo or a missing setup step
			// than a deliberate "no skills yet". Warn, not Error — the task
			// runs fine without skills.
			r.logger.WarnContext(ctx, "skills root unavailable, running without skills",
				"component", "agent_resolver",
				"agent_id", task.AgentID,
				"skills_root", skillsRoot,
			)
		}
	}
	agent := domain.Agent{
		ID:        firstNonEmptyAgentRuntimeResolver(agentCfg.ID, task.AgentID),
		CompanyID: task.CompanyID,
		Role:      agentCfg.Role,
		Status:    domain.AgentActive,
	}
	// Per-agent (worker) toolset: read-only workspace + task ledger + agent
	// messaging + web. This is deliberately a strict subset of the default
	// runtime's toolset (cli.defaultTaskRunner.RunTask), which additionally
	// carries session_search, moa_consult and delegate_task. Those three are
	// orchestrator-tier capabilities and are intentionally NOT granted here:
	//
	//   - delegate_task: the default runtime is the root orchestrator; workers
	//     spawning further workers would make the delegation tree unbounded.
	//   - session_search: MessageSearcher.SearchMessages/BrowseSessions query
	//     conversation history globally, with no company/agent filter. A worker
	//     is confined to agentToolRoot's sandbox and to the brief its delegator
	//     handed it; giving it unscoped cross-agent/cross-company history reads
	//     would breach that boundary.
	//   - moa_consult: high-risk and Sensitive, fanning out N+1 model calls to
	//     arbitrary MaaS profiles. A worker runs under exactly the profile its
	//     agent config assigns (agentCfg.MaasProfile); letting it consult other
	//     profiles would bypass that assignment and amplify cost per delegation.
	//
	// The asymmetry is the design, not an oversight — see
	// TestResolverOmitsOrchestratorOnlyTools, which locks it.
	toolRoot := agentToolRoot(r.rootConfig, agentCfg, task)
	tools := tool.NewFileReadWriteWorkspaceRegistry(toolRoot, r.audit,
		tool.WithAgentsInjection(r.rootConfig.ContextFiles.MaxFileChars, r.resolveHomeDir(ctx)),
		tool.WithProjectRoot(toolRoot))
	tool.RegisterTaskLedgerTools(tools, r.taskLedger)
	tool.RegisterAgentMessageTools(tools, r.messageStore)
	tool.RegisterWebTools(tools, webToolOptions(r.rootConfig.Web))
	if r.rootConfig.Browser.Enabled {
		brt, err := browser.NewRuntime(browser.RuntimeConfig{Headless: r.rootConfig.Browser.Headless, BinPath: r.rootConfig.Browser.BinPath})
		if err != nil {
			return domain.Agent{}, nil, false, fmt.Errorf("init browser runtime: %w", err)
		}
		tool.RegisterBrowserTools(tools, tool.BrowserToolOptions{Enabled: true, Runtime: brt})
	}
	recentTurns, err := r.recentTurnsForTask(ctx, task)
	if err != nil {
		return domain.Agent{}, nil, false, err
	}
	runner := NewRuntime(Config{
		Maas:                  maas.Client,
		Audit:                 r.audit,
		Events:                r.events,
		ContextBuilder:        contextBuilder,
		Tools:                 tools,
		ToolRoot:              toolRoot,
		MaxToolRounds:         r.rootConfig.Runtime.MaxToolRounds,
		LazyTools:             r.rootConfig.Runtime.LazyTools,
		Debug:                 r.rootConfig.Runtime.Debug,
		CompactTokenThreshold: r.rootConfig.Runtime.CompactTokenThreshold,
		Checkpoints:           r.checkpoints,
		ToolGate:              r.toolGate,
		Logger:                r.logger,
		CapabilitySkills:      capabilitySkills,
		SkillUsage:            r.skillUsage,
		DisabledTools:         agentCfg.DisabledTools,
		ConversationTurns:     recentTurns,
		EpisodeRecorder:       r.episodeRecorder,
	})
	return agent, runner, true, nil
}

// webToolOptions maps the web config block onto the tool package options.
func webToolOptions(cfg config.WebToolConfig) tool.WebToolOptions {
	return tool.WebToolOptions{
		Enabled:            cfg.Enabled,
		AllowPrivateHosts:  cfg.AllowPrivateHosts,
		Timeout:            time.Duration(cfg.TimeoutSeconds) * time.Second,
		MaxBytes:           int64(cfg.MaxResponseKB) * 1024,
		Allowlist:          cfg.Allowlist,
		SearxngURL:         cfg.SearxngURL,
		SearchEngine:       cfg.SearchEngine,
		SearchDefaultLimit: cfg.SearchDefaultLimit,
		SearchTimeout:      time.Duration(cfg.SearchTimeoutSeconds) * time.Second,
	}
}

// loadAgentContextFiles loads the resident context block for an agent.
// projectRoot is the agents.md project root (and upward ancestor chain
// anchor) — the caller passes agentToolRoot's result so agents.md tracks the
// same task.WorkingDir-first sandbox boundary as the tool registry. Root
// (childCfg.Root, falling back to rootCfg.ContextFiles.Root) stays the
// persona root for Soul/Tools/User/Memory, unaffected by projectRoot.
func loadAgentContextFiles(ctx context.Context, rootCfg config.Config, childCfg config.ContextFilesConfig, projectRoot string) (string, error) {
	if childCfg.Root == "" {
		childCfg.Root = rootCfg.ContextFiles.Root
	}
	block, err := contextfiles.Load(ctx, contextfiles.Config{
		Enabled:      childCfg.Enabled,
		Root:         childCfg.Root,
		ProjectRoot:  projectRoot,
		SoulPath:     childCfg.SoulPath,
		ToolsPath:    childCfg.ToolsPath,
		UserPath:     childCfg.UserPath,
		MemoryPath:   childCfg.MemoryPath,
		MaxFileChars: childCfg.MaxFileChars,
	})
	if err != nil {
		return "", err
	}
	return block.Render(), nil
}

// agentToolRoot resolves the tool-sandbox root (the WorkspacePathGuard root
// every read-only workspace tool built for this run is confined to). It
// prioritizes task.WorkingDir: when a task carries a non-empty working_dir
// (M3 per-task working directory), the agent's tools are sandboxed to that
// directory regardless of the agent's or root config's configured context
// root — the task's own working directory is the security boundary. Only
// when the task has no working_dir does it fall back to the pre-M3
// resolution: the agent's own ContextFiles.Root, else the root config's.
func agentToolRoot(rootCfg config.Config, agentCfg agentregistry.AgentConfig, task domain.Task) string {
	if wd := strings.TrimSpace(task.WorkingDir); wd != "" {
		return wd
	}
	if agentCfg.ContextFiles.Root != "" {
		return agentCfg.ContextFiles.Root
	}
	return rootCfg.ContextFiles.Root
}

// agentSkillsRoot picks the agent's own skills root, falling back to the root
// config's. TrimSpace, not a bare != "": a whitespace-only install_root is a
// typo, not a choice — treating it as "configured" would return a path that
// RootAvailable then rejects, silently losing the root config's skills instead
// of falling back to them.
func agentSkillsRoot(rootCfg config.Config, agentCfg agentregistry.AgentConfig) string {
	if root := strings.TrimSpace(agentCfg.Skills.InstallRoot); root != "" {
		return root
	}
	return rootCfg.Skills.InstallRoot
}

// sortedKeys returns the keys of a gateable-tool-name set sorted ascending, so
// a fail-loud "unknown disabled tool" error message lists the valid set in a
// stable, readable order rather than Go's randomized map iteration order.
func sortedKeys(names map[string]bool) []string {
	keys := make([]string, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func firstNonEmptyAgentRuntimeResolver(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
