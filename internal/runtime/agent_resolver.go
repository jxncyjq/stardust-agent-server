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
	"github.com/stardust/legion-agent/internal/prompt"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/skill"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/taskledger"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

type MaasRunnerFactoryResult struct {
	Client    port.MaasInferenceClient
	ModelName string
}

type MaasRunnerFactory func(profile string) (MaasRunnerFactoryResult, error)

// ConversationTurnLister loads a session's recent history in either of the two
// shapes G3 selects between. It is the set of methods the resolver needs from
// the session store, kept as its own interface so the runtime package does not
// depend on the whole store.
//
// Both projections live in ONE interface on purpose. They read the same events
// and a store can serve both; a store that could only do turns would turn G3
// into a switch that silently kept the old shape, which is exactly the "the
// seam is there but nobody calls it" failure this repo has hit twice. Declaring
// both here makes that a compile error at the wiring site instead.
type ConversationTurnLister interface {
	ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error)
	// ListConversationTranscript returns the same history as a provider
	// transcript (assistant with tool_calls, followed by the tool messages that
	// answer them). limit caps the messages returned, counting from the most
	// recent; see storage.SQLiteRepository.ListConversationTranscript for why a
	// cap may keep one or two extra messages rather than split an assistant
	// from its tool results.
	ListConversationTranscript(ctx context.Context, sessionID string, limit int) ([]port.InferenceMessage, error)
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

	// PluginSegments is the process-wide store of plugin-contributed prompt
	// blocks. It is the SAME store the plugin host writes to, so a plugin
	// mounted after this resolver was built still reaches the prompts of
	// every agent — the rendering happens per build, not at wiring time.
	//
	// Nil is a deployment with no plugin prompt segments, and costs nothing.
	PluginSegments *prompt.Segments

	// PluginTools is the registry mounted plugins contribute their tools to.
	// Every per-agent registry INHERITS from it, which is what puts a plugin's
	// tools in the model's reach and what makes the plugin's observe/decide
	// seams see an agent's own tool calls (both walk the parent chain).
	//
	// Nil is a deployment with no plugins.
	PluginTools *tool.Registry
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
	// Gate is the task-boundary gate every runtime this resolver builds
	// registers its tasks on. It is REQUIRED and must be the SAME gate the
	// plugin loader applies through: per-agent runtimes run tasks through the
	// same RunTask as the default one, so a resolver without the shared gate
	// would be a hole in the task-boundary contract exactly where per-agent
	// tasks run. NewAgentRuntimeResolver panics on a nil Gate.
	Gate *taskgate.TaskGate
	// BrowserRuntime is the ONE shared browser runtime (one Chromium process)
	// injected at serve assembly when RootConfig.Browser.Enabled. Per-agent
	// runtimes register browser_* against this shared instance rather than
	// launching a Chromium per task — launching per task leaked a browser
	// process on every worker task. Nil means browser tools are off.
	BrowserRuntime browser.RuntimeAPI
	// SessionEvents 是会话事件日志的落点，镜像默认运行时的 Config.SessionEvents。
	//
	// 它必须与默认运行时接**同一个** store：两条路径的任务落在同一批会话里（一个
	// 会话既可能派给具名 agent，也可能走默认 agent），只接一边会让那条会话的日志
	// 出现空洞——而「有洞的日志」与「这段时间什么都没发生」在数据上完全一样，
	// 谁也认不出来。本仓已经栽过两次「只接了 resolver、默认路径没接」的同形事故。
	//
	// nil 是契约允许的部署形态（非持久化驱动、测试构造），那时这些运行时整体不记
	// 事件（见 Config.SessionEvents），不是兜底。
	SessionEvents port.SessionEventStore
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
	pluginSegments    *prompt.Segments
	pluginTools       *tool.Registry
	logger            *slog.Logger
	skillUsage        SkillUsageRecorder
	conversationTurns ConversationTurnLister
	episodeRecorder   EpisodeRecorder
	browserRuntime    browser.RuntimeAPI
	gate              *taskgate.TaskGate
	sessionEvents     port.SessionEventStore
}

func NewAgentRuntimeResolver(cfg AgentRuntimeResolverConfig) *AgentRuntimeResolver {
	// Same fail-loud as NewRuntime, for the same reason: every runtime this
	// resolver builds runs tasks, and a missing gate would silently drop them
	// out of the task-boundary contract instead of failing.
	if cfg.Gate == nil {
		panic("runtime: NewAgentRuntimeResolver: Config.Gate is nil; per-agent runtimes without a " +
			"task-boundary gate would let a plugin change land in the middle of a running task")
	}
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
		pluginSegments:    cfg.PluginSegments,
		pluginTools:       cfg.PluginTools,
		logger:            cfg.Logger,
		skillUsage:        cfg.SkillUsage,
		conversationTurns: cfg.ConversationTurns,
		episodeRecorder:   cfg.EpisodeRecorder,
		browserRuntime:    cfg.BrowserRuntime,
		gate:              cfg.Gate,
		sessionEvents:     cfg.SessionEvents,
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

// sessionHistoryForTask loads the session history to inject into this task, in
// whichever shape G3 (Session.ToolTranscriptEnabled) selects: the recent turns
// the prompt renders as "Recent conversation:", or the provider transcript that
// carries the history's tool round-trips.
//
// A nil lister or an empty task.SessionID is a legitimate "no session history"
// state and yields an empty SessionHistory. A store failure is NOT: it returns
// an error, so a lost history is never mistaken for an empty one (CLAUDE.md
// fail-loud).
func (r *AgentRuntimeResolver) sessionHistoryForTask(ctx context.Context, task domain.Task) (SessionHistory, error) {
	// Delegated to the shared helper so this path and the CLI's
	// defaultTaskRunner cannot drift apart — wiring history into only one of
	// them is exactly how the GUI ended up with no cross-turn memory, and the
	// same hole would swallow the G3 switch.
	return SessionHistoryForTask(ctx, r.conversationTurns, r.rootConfig.Session, task)
}

func (r *AgentRuntimeResolver) ResolveTaskRunner(ctx context.Context, task domain.Task) (domain.Agent, TaskRunner, bool, error) {
	if r == nil || r.registry == nil || task.AgentID == "" {
		return domain.Agent{}, nil, false, nil
	}
	agentCfg, ok := r.registry.Get(task.AgentID)
	if !ok {
		return domain.Agent{}, nil, false, nil
	}
	// The zero DelegationContext is a task that arrives on its own rather than
	// under a parent: depth 0, the default spawn ceiling, and an empty
	// delegation role, which NewRuntime reads at depth 0 as the root
	// orchestrator. Everything past this line is shared with ResolveDelegate,
	// which passes the context its delegating runtime handed down instead.
	agent, runner, err := r.buildAgentRuntime(ctx, agentCfg, task, DelegationContext{})
	if err != nil {
		return domain.Agent{}, nil, false, err
	}
	return agent, runner, true, nil
}

// ResolveDelegate implements DelegationAgents: it builds the runtime a named
// sub-task runs on. The named agent's own configuration decides the
// tool-permission role, the disabled_tools deny-list, the MaaS profile, the
// context files and the tool sandbox; dc decides the delegation position, which
// is the delegating runtime's to decide and nothing the agent's configuration
// can override.
//
// A sub-task has no session, company or working directory of its own at this
// point, so the task it is built for carries only the agent id: the child gets
// that agent's configured workspace and starts with no session history, both of
// which are states the assembly below already treats as legitimate.
func (r *AgentRuntimeResolver) ResolveDelegate(ctx context.Context, id string, dc DelegationContext) (domain.Agent, *Runtime, error) {
	if r == nil || r.registry == nil {
		return domain.Agent{}, nil, fmt.Errorf("resolve delegate %q: this resolver has no agent registry", id)
	}
	agentCfg, ok := r.registry.Get(id)
	if !ok {
		return domain.Agent{}, nil, fmt.Errorf("resolve delegate %q: not a configured agent; configured agents are %v", id, r.AgentNames())
	}
	// A delegate is by definition below a parent. Depth 0 would make this child
	// the root of a delegation tree of its own: it would run its whole subtree
	// inside the ceiling budget its parent has already partly spent, and it
	// would register with the task-boundary gate as an arriving task instead of
	// as a child of one.
	if dc.Depth < 1 {
		return domain.Agent{}, nil, fmt.Errorf("resolve delegate %q: delegation depth %d is not below a parent", id, dc.Depth)
	}
	// normalizePositive, matching what NewRuntime does with the same field, so
	// the ceiling this refuses against is the ceiling the child would run under.
	if ceiling := normalizePositive(dc.MaxSpawnDepth, defaultMaxSpawnDepth); dc.Depth > ceiling {
		return domain.Agent{}, nil, fmt.Errorf("resolve delegate %q: delegation depth %d exceeds max spawn depth %d", id, dc.Depth, ceiling)
	}
	switch dc.Role {
	case "", roleLeaf, roleOrchestrator:
	default:
		return domain.Agent{}, nil, fmt.Errorf("resolve delegate %q: delegation role %q is not %q or %q", id, dc.Role, roleOrchestrator, roleLeaf)
	}
	// A named agent's own runtime never gets delegate_task registered on it:
	// buildAgentRuntime below assembles the tool registry itself, tool by tool,
	// and that assembly never calls RegisterDelegateTaskTool, which this
	// codebase calls exactly once, on the default runner's own root runtime
	// (internal/cli/command.go's defaultTaskRunner.RunTask). Accepting
	// roleOrchestrator here would set canDelegate() to true on a child that has
	// no delegate_task to call — a silently inert grant. Refuse it outright
	// instead of applying it and leaving it dead.
	//
	// This is the SECOND of two refusals, not the only one: the primary lives
	// in validateSubTaskSpec, which is what puts it inside RunSubTasks'
	// whole-batch pre-flight, so a batch containing this combination refuses
	// before any entry starts instead of failing halfway through. This copy
	// stays because ResolveDelegate is an exported DelegationAgents method
	// whose contract has to hold on its own rather than on validateSubTaskSpec
	// having run first — the same reason newSubRuntime re-checks a depth
	// ceiling validateSubTaskSpec already checked.
	if dc.Role == roleOrchestrator {
		return domain.Agent{}, nil, fmt.Errorf(
			"resolve delegate %q: delegation role %q cannot be combined with a named agent; a named agent's runtime never registers delegate_task, so it could never act as an orchestrator; delegate by name as a leaf, or delegate by role without a name",
			id, roleOrchestrator)
	}
	// dc.Toolsets, unlike Role above, is NOT refused for a named agent: it is
	// the delegating caller's one-time narrowing for this one delegation, and
	// buildAgentRuntime applies it on top of the named agent's own
	// agentCfg.DisabledTools deny-list rather than in place of it (spec §4.4) —
	// see the comment there. A name that does not match anything in the named
	// agent's own registry is silently dropped by tool.Registry.Subset, which
	// only narrows the child further; it can never widen it past what
	// buildAgentRuntime already assembled from agentCfg.
	return r.buildAgentRuntime(ctx, agentCfg, domain.Task{AgentID: id}, dc)
}

// buildAgentRuntime assembles one per-agent runtime. The split it enforces is
// the point of the function: agentCfg decides what running AS this agent means
// (tool-permission role, disabled_tools, MaaS profile, context files, skills
// root), task decides what this particular run is about (the working directory
// that becomes the tool sandbox root, the session whose history is injected,
// the company the returned domain.Agent belongs to), and dc decides where in a
// delegation tree the runtime sits.
//
// One function rather than one per caller: validating disabled_tools against
// the gateable set, resolving the MaaS profile and loading the context files
// are the same decisions however the runtime was asked for, and a second copy
// of them would be a second answer to them.
func (r *AgentRuntimeResolver) buildAgentRuntime(ctx context.Context, agentCfg agentregistry.AgentConfig, task domain.Task, dc DelegationContext) (domain.Agent, *Runtime, error) {
	// Fail-loud assembly-time validation (CLAUDE.md §0): a disabled_tools entry
	// that does not name a known gateable tool is a config error, not an inert
	// no-op — a typo would otherwise silently disable nothing. gateableNames is
	// fetched once, outside the loop, so validating N entries costs one map
	// build rather than N.
	gateableNames := toolauth.GateableToolNames()
	for _, name := range agentCfg.DisabledTools {
		if !gateableNames[name] {
			return domain.Agent{}, nil, fmt.Errorf(
				"agent %q disabled_tools names unknown tool %q (gateable: %v)",
				agentCfg.ID, name, sortedKeys(gateableNames))
		}
	}
	if r.maasFactory == nil {
		return domain.Agent{}, nil, fmt.Errorf("maas runner factory is nil")
	}
	maas, err := r.maasFactory(agentCfg.MaasProfile)
	if err != nil {
		return domain.Agent{}, nil, fmt.Errorf("create maas runner for profile %q: %w", agentCfg.MaasProfile, err)
	}
	contextBlock, err := loadAgentContextFiles(ctx, r.rootConfig, agentCfg.ContextFiles, agentToolRoot(r.rootConfig, agentCfg, task))
	if err != nil {
		return domain.Agent{}, nil, fmt.Errorf("load agent context files for %q: %w", task.AgentID, err)
	}
	contextBuilder := cognitive.NewCore(cognitive.NoopCompressor{}).
		WithContextFiles(contextBlock).
		WithPluginSegments(r.pluginSegments)
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
		tool.WithProjectRoot(toolRoot),
		tool.WithPluginTools(r.pluginTools))
	tool.RegisterTaskLedgerTools(tools, r.taskLedger)
	tool.RegisterAgentMessageTools(tools, r.messageStore)
	tool.RegisterWebTools(tools, webToolOptions(r.rootConfig.Web))
	if r.browserRuntime != nil {
		// Shared runtime injected at serve assembly; no per-task browser launch.
		tool.RegisterBrowserTools(tools, tool.BrowserToolOptions{Enabled: true, Runtime: r.browserRuntime, ToolRoot: toolRoot})
	}
	// dc.Toolsets is the delegating caller's one-time narrowing for THIS
	// delegation (spec §4.4); agentCfg.DisabledTools, passed to Config below,
	// is this agent's own standing deny-list, applied separately on every call
	// by Runtime.effectiveTools (tools.Without(r.disabledTools...)). The two
	// stack rather than either replacing the other: baking Toolsets in here,
	// ahead of a DisabledTools filter that runs later on whatever this
	// produces, means the tools this runtime ever offers are the caller's
	// requested subset MINUS the agent's own denials -- neither side can widen
	// past what the other already narrowed. Subset drops any name it does not
	// recognise rather than erroring, so this step can only ever narrow the
	// registry built above, never widen it.
	//
	// This must run before SetAskArbiter below: Subset returns a NEW view that
	// does not carry the base registry's askArbiter field (tool.Registry.view
	// copies policy/enforcer/guards/audit/sanitizer but not askArbiter), so
	// setting the arbiter first would leave it stranded on a registry this
	// runtime no longer uses once Toolsets narrows tools to the view.
	if len(dc.Toolsets) > 0 {
		tools = tools.Subset(dc.Toolsets...)
	}
	// A plugin granted the decide extension may answer "ask", and the ticket
	// that answers it is read at DISPATCH time, in this registry. The gate that
	// opens those tickets is the only thing that can read them back, so it is
	// installed here as the arbiter — a registry without one refuses every ask,
	// including the ones a human already approved.
	if arbiter, ok := r.toolGate.(tool.AskArbiter); ok {
		tools.SetAskArbiter(arbiter)
	}
	history, err := r.sessionHistoryForTask(ctx, task)
	if err != nil {
		return domain.Agent{}, nil, err
	}
	// Suspend/resume is wiring for a task that ARRIVED under an id something
	// outside this run holds and can come back to. A sub-task's id is minted
	// per delegation inside the parent's own call, so a checkpoint written
	// under it is one nothing ever loads, and a suspension ends the delegation
	// with no path back into it. checkSuspend needs both checkpoints and
	// toolGate to do anything at all, so dropping either one already disarms
	// it; the dispatch-time gate call in lazytools.go's dispatchToolCall needs
	// only toolGate, but that path is harmless to drop too, because
	// r.toolGate.Resolve's ManualToolGate implementation returns allow=true on
	// its first line whenever task.Mode is not domain.ModeManual, and
	// delegation.go's runChild never sets Mode on the domain.Task it builds —
	// a delegated child is always Auto (see the invariant comment on that
	// check in runtime.go's RunTask). A cloned child carries neither field
	// either — a named child answers this the same way rather than a third
	// way. The ask arbiter set on the tool registry above is a different thing
	// and is unaffected.
	checkpoints, toolGate := r.checkpoints, r.toolGate
	if dc.Depth > 0 {
		checkpoints, toolGate = nil, nil
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
		Checkpoints:           checkpoints,
		ToolGate:              toolGate,
		Logger:                r.logger,
		CapabilitySkills:      capabilitySkills,
		SkillUsage:            r.skillUsage,
		DisabledTools:         agentCfg.DisabledTools,
		// 两者是同一个选择的两半，SessionHistoryForTask 只会填其中一个；两个都传，
		// 是为了让「开关选了哪一边」在这里没有第二次做主的机会。
		ConversationTurns: history.Turns,
		HistoryTranscript: history.Transcript,
		// Deliberate, not an oversight: every runtime this resolver builds off
		// agentCfg -- a top-level per-agent task as much as a named delegate --
		// runs AS that agent, and writing one episodic-memory record per finished
		// task is part of what running as a configured agent means here. This is
		// the one field where a named delegate and a cloned delegate (newSubRuntime
		// in delegation.go, which carries no episodeRecorder at all) genuinely
		// diverge: a named sub-task's one-off id still gets its own episode, a
		// cloned sub-task's does not. See
		// TestResolveDelegateCarriesTheResolverEpisodeRecorder and
		// TestClonedSubRuntimeCarriesNoEpisodeRecorder, which lock both halves of
		// that asymmetry down.
		EpisodeRecorder: r.episodeRecorder,
		Gate:            r.gate,
		SessionEvents:   r.sessionEvents,
		// The per-agent runtime this resolver builds can itself delegate, and it
		// must resolve names against the same registry this resolver already
		// wraps -- passing itself here (AgentRuntimeResolver implements
		// DelegationAgents, whose three methods are all defined in this file)
		// backs that delegation without a second registry reference anywhere.
		DelegationAgents: r,
		// Where this runtime sits in a delegation tree is dc's to say, and only
		// dc's: an agent's configuration says what running AS that agent means,
		// and the same agent can be both a top-level task's runner and someone
		// else's child. Note Role here is the DELEGATION role (may this runtime
		// delegate further), a different question from agent.Role above, which
		// is the tool-permission role agentCfg carries.
		Role:          dc.Role,
		Depth:         dc.Depth,
		MaxSpawnDepth: dc.MaxSpawnDepth,
		// 这个 agent 跑在哪个档位上，与上面 r.maasFactory(agentCfg.MaasProfile) 选
		// 客户端用的是同一个解析顺序，所以轨迹里记的名字与真正被调用的客户端一致。
		ModelProfile: r.rootConfig.Maas.ResolveProfileName(agentCfg.MaasProfile),
	})
	return agent, runner, nil
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

// AgentNames implements DelegationAgents.
func (r *AgentRuntimeResolver) AgentNames() []string {
	names := r.registry.Names()
	sort.Strings(names)
	return names
}

// HasAgent implements DelegationAgents.
func (r *AgentRuntimeResolver) HasAgent(id string) bool {
	_, ok := r.registry.Get(id)
	return ok
}

func firstNonEmptyAgentRuntimeResolver(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
