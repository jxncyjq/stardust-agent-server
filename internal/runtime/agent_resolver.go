package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/contextfiles"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
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
}

type AgentRuntimeResolver struct {
	registry     *agentregistry.Registry
	rootConfig   config.Config
	audit        port.AuditLog
	events       port.EventBus
	taskLedger   *taskledger.Ledger
	messageStore tool.AgentMessageStore
	maasFactory  MaasRunnerFactory
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
	if skillsRoot := agentSkillsRoot(r.rootConfig, agentCfg); skillsRoot != "" {
		contextBuilder = contextBuilder.WithSkills(skill.NewSystem(skill.Config{
			Roots:   []string{skillsRoot},
			Scanner: skill.NewSecurityScanner(),
		}))
	}
	agent := domain.Agent{
		ID:        firstNonEmptyAgentRuntimeResolver(agentCfg.ID, task.AgentID),
		CompanyID: task.CompanyID,
		Role:      agentCfg.Role,
		Status:    domain.AgentActive,
	}
	tools := tool.NewReadOnlyWorkspaceRegistry(agentToolRoot(r.rootConfig, agentCfg), r.audit)
	tool.RegisterTaskLedgerTools(tools, r.taskLedger)
	tool.RegisterAgentMessageTools(tools, r.messageStore)
	tool.RegisterWebTools(tools, webToolOptions(r.rootConfig.Web))
	runner := NewRuntime(Config{
		Maas:           maas.Client,
		Audit:          r.audit,
		Events:         r.events,
		ContextBuilder: contextBuilder,
		Tools:          tools,
		MaxToolRounds:  r.rootConfig.Runtime.MaxToolRounds,
		LazyTools:      r.rootConfig.Runtime.LazyTools,
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

func agentToolRoot(rootCfg config.Config, agentCfg agentregistry.AgentConfig) string {
	if agentCfg.ContextFiles.Root != "" {
		return agentCfg.ContextFiles.Root
	}
	return rootCfg.ContextFiles.Root
}

func agentSkillsRoot(rootCfg config.Config, agentCfg agentregistry.AgentConfig) string {
	if agentCfg.Skills.InstallRoot != "" {
		return agentCfg.Skills.InstallRoot
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
