package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/agentregistry"
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
)

type MaasRunnerFactoryResult struct {
	Client    port.MaasInferenceClient
	ModelName string
}

type MaasRunnerFactory func(profile string) (MaasRunnerFactoryResult, error)

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
}

type AgentRuntimeResolver struct {
	registry     *agentregistry.Registry
	rootConfig   config.Config
	audit        port.AuditLog
	events       port.EventBus
	taskLedger   *taskledger.Ledger
	messageStore tool.AgentMessageStore
	maasFactory  MaasRunnerFactory
	checkpoints  *sessionstate.Store
	toolGate     ToolGate
	logger       *slog.Logger
	skillUsage   SkillUsageRecorder
}

func NewAgentRuntimeResolver(cfg AgentRuntimeResolverConfig) *AgentRuntimeResolver {
	return &AgentRuntimeResolver{
		registry:     cfg.Registry,
		rootConfig:   cfg.RootConfig,
		audit:        cfg.Audit,
		events:       cfg.Events,
		taskLedger:   cfg.TaskLedger,
		messageStore: cfg.MessageStore,
		maasFactory:  cfg.MaasFactory,
		checkpoints:  cfg.Checkpoints,
		toolGate:     cfg.ToolGate,
		logger:       cfg.Logger,
		skillUsage:   cfg.SkillUsage,
	}
}

func (r *AgentRuntimeResolver) ResolveTaskRunner(ctx context.Context, task domain.Task) (domain.Agent, TaskRunner, bool, error) {
	if r == nil || r.registry == nil || task.AgentID == "" {
		return domain.Agent{}, nil, false, nil
	}
	agentCfg, ok := r.registry.Get(task.AgentID)
	if !ok {
		return domain.Agent{}, nil, false, nil
	}
	if r.maasFactory == nil {
		return domain.Agent{}, nil, false, fmt.Errorf("maas runner factory is nil")
	}
	maas, err := r.maasFactory(agentCfg.MaasProfile)
	if err != nil {
		return domain.Agent{}, nil, false, fmt.Errorf("create maas runner for profile %q: %w", agentCfg.MaasProfile, err)
	}
	contextBlock, err := loadAgentContextFiles(ctx, r.rootConfig, agentCfg.ContextFiles)
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
	tools := tool.NewFileReadOnlyWorkspaceRegistry(agentToolRoot(r.rootConfig, agentCfg, task), r.audit)
	tool.RegisterTaskLedgerTools(tools, r.taskLedger)
	tool.RegisterAgentMessageTools(tools, r.messageStore)
	tool.RegisterWebTools(tools, webToolOptions(r.rootConfig.Web))
	runner := NewRuntime(Config{
		Maas:             maas.Client,
		Audit:            r.audit,
		Events:           r.events,
		ContextBuilder:   contextBuilder,
		Tools:            tools,
		MaxToolRounds:    r.rootConfig.Runtime.MaxToolRounds,
		LazyTools:        r.rootConfig.Runtime.LazyTools,
		Checkpoints:      r.checkpoints,
		ToolGate:         r.toolGate,
		Logger:           r.logger,
		CapabilitySkills: capabilitySkills,
		SkillUsage:       r.skillUsage,
	})
	return agent, runner, true, nil
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

func loadAgentContextFiles(ctx context.Context, rootCfg config.Config, childCfg config.ContextFilesConfig) (string, error) {
	if childCfg.Root == "" {
		childCfg.Root = rootCfg.ContextFiles.Root
	}
	block, err := contextfiles.Load(ctx, contextfiles.Config{
		Enabled:      childCfg.Enabled,
		Root:         childCfg.Root,
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

func firstNonEmptyAgentRuntimeResolver(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
