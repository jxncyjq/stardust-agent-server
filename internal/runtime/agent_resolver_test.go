package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/manualgate"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/testsupport"
)

// TestResolverInjectsCheckpointsAndGate guards Task 7's resolver wiring: a
// resolver constructed with Checkpoints/ToolGate must pass both through to
// every per-agent *Runtime it builds (not just the default runtime), so
// Manual-mode suspend/resume works for delegated/child agents too.
func TestResolverInjectsCheckpointsAndGate(t *testing.T) {
	t.Parallel()

	cfgStore := sessionstate.NewStore(t.TempDir())
	gate := manualgate.New(approval.NewToolGateStore(t.TempDir()))
	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {ID: "agent-researcher", Role: "researcher", MaasProfile: "deep"},
		}),
		RootConfig: config.Config{Runtime: config.RuntimeConfig{MaxToolRounds: 1}},
		Audit:      adapter.NewMemoryAuditLog(),
		Events:     adapter.NewMemoryEventBus(),
		MaasFactory: func(string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: &resolverCaptureMaas{response: "ok"}}, nil
		},
		Checkpoints: cfgStore,
		ToolGate:    gate,
	})

	_, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), domain.Task{
		ID:      "task-gate",
		AgentID: "researcher",
	})
	if err != nil {
		t.Fatalf("ResolveTaskRunner error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ResolveTaskRunner ok = false, want true")
	}
	rt, isRuntime := runner.(*Runtime)
	if !isRuntime {
		t.Fatalf("runner type = %T, want *Runtime", runner)
	}
	if rt.checkpoints == nil {
		t.Fatalf("resolver runtime missing checkpoints, want non-nil (cfgStore)")
	}
	if rt.checkpoints != cfgStore {
		t.Fatalf("resolver runtime checkpoints = %p, want the same store %p", rt.checkpoints, cfgStore)
	}
	if rt.toolGate == nil {
		t.Fatalf("resolver runtime missing toolGate, want non-nil (gate)")
	}
	if rt.toolGate != ToolGate(gate) {
		t.Fatalf("resolver runtime toolGate = %v, want the same gate %v", rt.toolGate, gate)
	}
}

// TestResolverInjectsSkillUsage guards the per-agent half of the I-1 fix: a
// resolver constructed with SkillUsage must pass it through to every per-agent
// *Runtime it builds, mirroring TestResolverInjectsCheckpointsAndGate. Without
// this wiring, dispatchLoadCapabilities's `if r.skillUsage != nil { Touch }`
// (internal/runtime/lazytools.go) silently no-ops for every per-agent task, and
// the Curator (internal/skill/curator.go:153) never ages skills those agents
// load because they carry no usage history.
func TestResolverInjectsSkillUsage(t *testing.T) {
	t.Parallel()

	usage := &recordingUsage{}
	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {ID: "agent-researcher", Role: "researcher", MaasProfile: "deep"},
		}),
		RootConfig: config.Config{Runtime: config.RuntimeConfig{MaxToolRounds: 1}},
		Audit:      adapter.NewMemoryAuditLog(),
		Events:     adapter.NewMemoryEventBus(),
		MaasFactory: func(string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: &resolverCaptureMaas{response: "ok"}}, nil
		},
		SkillUsage: usage,
	})

	_, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), domain.Task{
		ID:      "task-skill-usage",
		AgentID: "researcher",
	})
	if err != nil {
		t.Fatalf("ResolveTaskRunner error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ResolveTaskRunner ok = false, want true")
	}
	rt, isRuntime := runner.(*Runtime)
	if !isRuntime {
		t.Fatalf("runner type = %T, want *Runtime", runner)
	}
	if rt.skillUsage == nil {
		t.Fatalf("resolver runtime missing skillUsage, want non-nil (usage)")
	}
	if rt.skillUsage != SkillUsageRecorder(usage) {
		t.Fatalf("resolver runtime skillUsage = %v, want the same recorder %v", rt.skillUsage, usage)
	}
}

// TestAgentToolRootPrefersTaskWorkingDir guards Task 7's sandbox-root
// priority: a task carrying a non-empty WorkingDir always wins over both the
// agent's and the root config's configured ContextFiles.Root, since the
// task's working directory is the security boundary the tool sandbox
// (WorkspacePathGuard) must be confined to.
func TestAgentToolRootPrefersTaskWorkingDir(t *testing.T) {
	t.Parallel()

	wd := t.TempDir()
	got := agentToolRoot(rootCfgWithContextRoot("/ctx"), agentCfgWithContextRoot(""), domain.Task{WorkingDir: wd})
	if got != wd {
		t.Fatalf("agentToolRoot = %q, want task.WorkingDir %q", got, wd)
	}
}

// TestAgentToolRootFallsBackWhenNoWorkingDir guards the pre-M3 fallback: an
// empty task.WorkingDir must not disturb the existing agent-then-root
// ContextFiles.Root resolution.
func TestAgentToolRootFallsBackWhenNoWorkingDir(t *testing.T) {
	t.Parallel()

	got := agentToolRoot(rootCfgWithContextRoot("/ctx"), agentCfgWithContextRoot(""), domain.Task{})
	if got != "/ctx" {
		t.Fatalf("agentToolRoot = %q, want /ctx fallback", got)
	}
}

func rootCfgWithContextRoot(root string) config.Config {
	return config.Config{ContextFiles: config.ContextFilesConfig{Root: root}}
}

func agentCfgWithContextRoot(root string) agentregistry.AgentConfig {
	return agentregistry.AgentConfig{ContextFiles: config.ContextFilesConfig{Root: root}}
}

func TestAgentRuntimeResolverUsesRegisteredAgentMaasProfileAndContextFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeResolverContextFile(t, root, "researcher/SOUL.md", "researcher SOUL context")
	writeResolverContextFile(t, root, "researcher/TOOLS.md", "researcher TOOLS context")
	writeResolverContextFile(t, root, "researcher/USER.md", "researcher USER context")
	writeResolverContextFile(t, root, "researcher/MEMORY.md", "researcher MEMORY context")

	maas := &resolverCaptureMaas{response: "researched"}
	var gotProfile string
	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {
				ID:          "agent-researcher",
				Role:        "researcher",
				MaasProfile: "deep",
				ContextFiles: config.ContextFilesConfig{
					Enabled:      true,
					SoulPath:     "researcher/SOUL.md",
					ToolsPath:    "researcher/TOOLS.md",
					UserPath:     "researcher/USER.md",
					MemoryPath:   "researcher/MEMORY.md",
					MaxFileChars: 20000,
				},
			},
		}),
		RootConfig: config.Config{
			ContextFiles: config.ContextFilesConfig{Root: root},
			Runtime:      config.RuntimeConfig{MaxToolRounds: 1},
		},
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
		MaasFactory: func(profile string) (MaasRunnerFactoryResult, error) {
			gotProfile = profile
			return MaasRunnerFactoryResult{Client: maas, ModelName: "deep-model"}, nil
		},
	})

	agent, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), domain.Task{
		ID:        "task-research",
		CompanyID: "company-1",
		AgentID:   "researcher",
		Input:     "map the current design",
	})
	if err != nil {
		t.Fatalf("ResolveTaskRunner(researcher) error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ResolveTaskRunner(researcher) ok = false, want true")
	}
	if gotProfile != "deep" {
		t.Fatalf("MaasRunnerFactory profile = %q, want %q", gotProfile, "deep")
	}
	if agent.ID != "agent-researcher" {
		t.Errorf("ResolveTaskRunner(researcher) agent.ID = %q, want agent-researcher", agent.ID)
	}
	if agent.CompanyID != "company-1" {
		t.Errorf("ResolveTaskRunner(researcher) agent.CompanyID = %q, want company-1", agent.CompanyID)
	}
	if agent.Role != "researcher" {
		t.Errorf("ResolveTaskRunner(researcher) agent.Role = %q, want researcher", agent.Role)
	}
	if agent.Status != domain.AgentActive {
		t.Errorf("ResolveTaskRunner(researcher) agent.Status = %q, want %q", agent.Status, domain.AgentActive)
	}

	run, err := runner.RunTask(context.Background(), agent, domain.Task{
		ID:        "task-research",
		CompanyID: "company-1",
		AgentID:   "researcher",
		Input:     "map the current design",
	})
	if err != nil {
		t.Fatalf("RunTask(researcher runtime) error = %v, want nil", err)
	}
	if run.Result != "researched" {
		t.Fatalf("RunTask(researcher runtime).Result = %q, want researched", run.Result)
	}
	for _, want := range []string{
		"Role: researcher",
		"researcher SOUL context",
		"researcher TOOLS context",
		"researcher USER context",
		"researcher MEMORY context",
		"map the current design",
	} {
		if !strings.Contains(maas.prompt, want) {
			t.Fatalf("RunTask(researcher runtime) prompt missing %q:\n%s", want, maas.prompt)
		}
	}
}

func TestAgentRuntimeResolverMountsRoleScopedSkills(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeResolverSkill(t, root, "skills/researcher/go-research/SKILL.md", `---
id: go-research
name: Go Research
version: 1.0.0
status: enabled
tags: research,cache
summary: Use evidence-first cache research.
---
Use evidence-first cache research.
`)
	writeResolverSkill(t, root, "skills/writer/go-writing/SKILL.md", `---
id: go-writing
name: Go Writing
version: 1.0.0
status: enabled
tags: write,cache
summary: Write concise cache documentation.
---
Write concise cache documentation.
`)

	maas := &resolverCaptureMaas{response: "researched"}
	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {
				ID:           "researcher",
				Role:         "researcher",
				MaasProfile:  "deep",
				ContextFiles: config.ContextFilesConfig{Root: root},
				Skills:       config.SkillsConfig{InstallRoot: filepath.Join(root, "skills", "researcher")},
			},
			"writer": {
				ID:           "writer",
				Role:         "writer",
				MaasProfile:  "deep",
				ContextFiles: config.ContextFilesConfig{Root: root},
				Skills:       config.SkillsConfig{InstallRoot: filepath.Join(root, "skills", "writer")},
			},
		}),
		RootConfig: config.Config{
			ContextFiles: config.ContextFilesConfig{Root: root},
			Runtime:      config.RuntimeConfig{MaxToolRounds: 1, LazyTools: true},
			Skills:       config.SkillsConfig{InstallRoot: filepath.Join(root, "skills", "global")},
		},
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
		MaasFactory: func(profile string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: maas, ModelName: profile + "-model"}, nil
		},
	})

	agent, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), domain.Task{
		ID:      "task-research-skill",
		AgentID: "researcher",
		Input:   "research cache behavior",
	})
	if err != nil {
		t.Fatalf("ResolveTaskRunner(researcher skills) error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ResolveTaskRunner(researcher skills) ok = false, want true")
	}
	if _, err := runner.RunTask(context.Background(), agent, domain.Task{
		ID:      "task-research-skill",
		AgentID: "researcher",
		Input:   "research cache behavior",
	}); err != nil {
		t.Fatalf("RunTask(researcher skills) error = %v, want nil", err)
	}
	if !strings.Contains(maas.prompt, "go-research") || !strings.Contains(maas.prompt, "Use evidence-first cache research.") {
		t.Fatalf("RunTask(researcher skills) prompt missing researcher skill:\n%s", maas.prompt)
	}
	if strings.Contains(maas.prompt, "go-writing") {
		t.Fatalf("RunTask(researcher skills) prompt contains writer skill:\n%s", maas.prompt)
	}
}

// A whitespace-only install_root is a typo, not a choice. Treating it as
// "configured" would hand RootAvailable a path it rejects, so the agent would
// end up with no skills at all instead of inheriting the root config's.
func TestAgentSkillsRootFallsBackOnBlankAgentConfig(t *testing.T) {
	t.Parallel()

	rootCfg := config.Config{Skills: config.SkillsConfig{InstallRoot: "root-skills"}}

	for _, blank := range []string{"", "   ", "\t"} {
		agentCfg := agentregistry.AgentConfig{Skills: config.SkillsConfig{InstallRoot: blank}}
		if got := agentSkillsRoot(rootCfg, agentCfg); got != "root-skills" {
			t.Errorf("agentSkillsRoot(install_root=%q) = %q, want the root config's %q", blank, got, "root-skills")
		}
	}

	own := agentregistry.AgentConfig{Skills: config.SkillsConfig{InstallRoot: "  agent-skills  "}}
	if got := agentSkillsRoot(rootCfg, own); got != "agent-skills" {
		t.Errorf("agentSkillsRoot(padded own root) = %q, want %q", got, "agent-skills")
	}
}

// Regression: a sub-agent whose skills install_root does not exist yet (nothing
// has been installed) must still run. The default runtime guards the mount with
// an existence check, but the resolver only tested the path for being a
// non-empty string, so the skill walk hit a missing directory and EVERY task
// routed to ANY configured sub-agent failed while building its context:
//
//	run task: build cognitive context: select task skills:
//	scan skills in "skills/researcher": ... cannot find the path specified.
//
// An uninstalled skills directory is a valid empty state, not a failure.
func TestAgentRuntimeResolverToleratesMissingSkillsRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Deliberately never created — this is the "no skills installed yet" state.
	missingSkills := filepath.Join(root, "skills", "researcher")

	maas := &resolverCaptureMaas{response: "researched"}
	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {
				ID:           "researcher",
				Role:         "researcher",
				MaasProfile:  "deep",
				ContextFiles: config.ContextFilesConfig{Root: root},
				Skills:       config.SkillsConfig{InstallRoot: missingSkills},
			},
		}),
		RootConfig: config.Config{
			ContextFiles: config.ContextFilesConfig{Root: root},
			Runtime:      config.RuntimeConfig{MaxToolRounds: 1},
		},
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
		MaasFactory: func(profile string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: maas, ModelName: profile + "-model"}, nil
		},
	})

	task := domain.Task{ID: "task-no-skills", AgentID: "researcher", Input: "research cache behavior"}
	agent, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), task)
	if err != nil {
		t.Fatalf("ResolveTaskRunner(missing skills root) error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ResolveTaskRunner(missing skills root) ok = false, want true")
	}
	if _, err := runner.RunTask(context.Background(), agent, task); err != nil {
		t.Fatalf("RunTask(missing skills root) error = %v, want nil (an uninstalled skills dir is a valid empty state)", err)
	}
}

func TestAgentRuntimeResolverRoleSkillsInheritRootWhenUnset(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeResolverSkill(t, root, "skills/global/go-shared/SKILL.md", `---
id: go-shared
name: Go Shared
version: 1.0.0
status: enabled
tags: cache
summary: Shared cache skill.
---
Shared cache skill.
`)
	maas := &resolverCaptureMaas{response: "ok"}
	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"writer": {
				ID:           "writer",
				Role:         "writer",
				MaasProfile:  "deep",
				ContextFiles: config.ContextFilesConfig{Root: root},
			},
		}),
		RootConfig: config.Config{
			ContextFiles: config.ContextFilesConfig{Root: root},
			Runtime:      config.RuntimeConfig{MaxToolRounds: 1, LazyTools: true},
			Skills:       config.SkillsConfig{InstallRoot: filepath.Join(root, "skills", "global")},
		},
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
		MaasFactory: func(profile string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: maas, ModelName: profile + "-model"}, nil
		},
	})

	agent, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), domain.Task{
		ID:      "task-shared-skill",
		AgentID: "writer",
		Input:   "cache summary",
	})
	if err != nil {
		t.Fatalf("ResolveTaskRunner(writer inherited skills) error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ResolveTaskRunner(writer inherited skills) ok = false, want true")
	}
	if _, err := runner.RunTask(context.Background(), agent, domain.Task{ID: "task-shared-skill", AgentID: "writer", Input: "cache summary"}); err != nil {
		t.Fatalf("RunTask(writer inherited skills) error = %v, want nil", err)
	}
	if !strings.Contains(maas.prompt, "go-shared") {
		t.Fatalf("RunTask(writer inherited skills) prompt missing inherited skill:\n%s", maas.prompt)
	}
}

func TestAgentRuntimeResolverRegistryMissReturnsFalse(t *testing.T) {
	t.Parallel()

	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{}),
		MaasFactory: func(string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: &resolverCaptureMaas{}}, nil
		},
	})
	agent, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), domain.Task{
		ID:      "task-missing",
		AgentID: "missing",
	})
	if err != nil {
		t.Fatalf("ResolveTaskRunner(registry miss) error = %v, want nil", err)
	}
	if ok {
		t.Fatalf("ResolveTaskRunner(registry miss) ok = true, want false")
	}
	if agent != (domain.Agent{}) {
		t.Fatalf("ResolveTaskRunner(registry miss) agent = %#v, want zero value", agent)
	}
	if runner != nil {
		t.Fatalf("ResolveTaskRunner(registry miss) runner = %#v, want nil", runner)
	}
}

func TestAgentRuntimeResolverFactoryErrorReturnsError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("factory failed")
	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {ID: "agent-researcher", Role: "researcher", MaasProfile: "deep"},
		}),
		MaasFactory: func(string) (MaasRunnerFactoryResult, error) { return MaasRunnerFactoryResult{}, wantErr },
	})
	_, _, ok, err := resolver.ResolveTaskRunner(context.Background(), domain.Task{
		ID:      "task-error",
		AgentID: "researcher",
	})
	if err == nil {
		t.Fatalf("ResolveTaskRunner(factory error) error = nil, want error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("ResolveTaskRunner(factory error) error = %v, want %v", err, wantErr)
	}
	if ok {
		t.Fatalf("ResolveTaskRunner(factory error) ok = true, want false")
	}
}

// TestResolverOmitsOrchestratorOnlyTools locks the intentional asymmetry
// between the per-agent (worker) toolset built by ResolveTaskRunner and the
// default runtime's toolset (cli.defaultTaskRunner.RunTask): workers get the
// read-only workspace, task ledger, agent messaging and web tools, but never
// the orchestrator-tier session_search / moa_consult / delegate_task. See the
// rationale comment in ResolveTaskRunner — unbounded delegation, unscoped
// cross-agent history reads, and MaaS-profile bypass respectively.
func TestResolverOmitsOrchestratorOnlyTools(t *testing.T) {
	t.Parallel()

	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {ID: "agent-researcher", Role: "researcher", MaasProfile: "deep"},
		}),
		RootConfig: config.Config{
			ContextFiles: config.ContextFilesConfig{Root: t.TempDir()},
			Runtime:      config.RuntimeConfig{MaxToolRounds: 1},
			Web:          config.WebToolConfig{Enabled: true},
		},
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
		MaasFactory: func(string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: &resolverCaptureMaas{response: "ok"}}, nil
		},
	})

	_, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), domain.Task{
		ID:      "task-tools",
		AgentID: "researcher",
	})
	if err != nil {
		t.Fatalf("ResolveTaskRunner error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ResolveTaskRunner ok = false, want true")
	}
	rt, isRuntime := runner.(*Runtime)
	if !isRuntime {
		t.Fatalf("runner type = %T, want *Runtime", runner)
	}
	if rt.tools == nil {
		t.Fatalf("resolver runtime tools = nil, want a registry")
	}
	names := make(map[string]bool)
	for _, descriptor := range rt.tools.Descriptors() {
		names[descriptor.Name] = true
	}
	for _, want := range []string{"read_file", "fetch_url"} {
		if !names[want] {
			t.Fatalf("per-agent registry missing worker tool %q, have %v", want, names)
		}
	}
	for _, forbidden := range []string{"session_search", "moa_consult", "delegate_task"} {
		if names[forbidden] {
			t.Fatalf("per-agent registry registers orchestrator-only tool %q, want it omitted", forbidden)
		}
	}
}

// TestResolverGivesWorkerWriteFile locks that per-agent (worker) tasks can
// write files: workers build NewFileReadWriteWorkspaceRegistry, so write_file
// must be present (sandboxed to the agent's working dir, still Sensitive). This
// is the per-agent half of giving serve tasks write capability; the CLI default
// runner is covered in package cli.
func TestResolverGivesWorkerWriteFile(t *testing.T) {
	t.Parallel()

	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {ID: "agent-researcher", Role: "researcher", MaasProfile: "deep"},
		}),
		RootConfig: config.Config{
			ContextFiles: config.ContextFilesConfig{Root: t.TempDir()},
			Runtime:      config.RuntimeConfig{MaxToolRounds: 1},
		},
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
		MaasFactory: func(string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: &resolverCaptureMaas{response: "ok"}}, nil
		},
	})

	_, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), domain.Task{
		ID:      "task-write",
		AgentID: "researcher",
	})
	if err != nil || !ok {
		t.Fatalf("ResolveTaskRunner = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	rt, isRuntime := runner.(*Runtime)
	if !isRuntime {
		t.Fatalf("runner type = %T, want *Runtime", runner)
	}
	names := make(map[string]bool)
	for _, descriptor := range rt.tools.Descriptors() {
		names[descriptor.Name] = true
	}
	if !names["write_file"] {
		t.Fatalf("per-agent registry missing write_file, have %v", names)
	}
}

type resolverCaptureMaas struct {
	response string
	prompt   string
}

func (m *resolverCaptureMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompt = testsupport.RequestText(req)
	return port.InferenceResponse{Text: m.response}, nil
}

func writeResolverContextFile(t *testing.T, root string, rel string, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v, want nil", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}
}

func writeResolverSkill(t *testing.T, root string, rel string, content string) {
	t.Helper()
	writeResolverContextFile(t, root, rel, content)
}
