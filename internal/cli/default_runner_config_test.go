package cli

import (
	"testing"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	agentruntime "github.com/stardust/legion-agent/internal/runtime"
	"github.com/stardust/legion-agent/internal/skill"
)

// TestBuildDefaultRunnerConfigWiresSkillUsage guards the default-runner half of
// the I-1 fix (final-review.md): BuildServeService's defaultRunner.runtimeCfg
// must carry the shared *skill.UsageStore through to the runtime as
// Config.SkillUsage, or dispatchLoadCapabilities's
// `if r.skillUsage != nil { Touch }` (internal/runtime/lazytools.go) silently
// no-ops for every default-runtime task, and the Curator
// (internal/skill/curator.go:153, "无使用记录的技能不会被动") never ages any
// skill the default runner loads.
//
// This exercises buildDefaultRunnerConfig, the assembly function
// BuildServeService calls to build that Config, rather than the full
// BuildServeService (which needs a listener, storage, and a MaaS client and
// does not expose runtimeCfg for inspection).
func TestBuildDefaultRunnerConfigWiresSkillUsage(t *testing.T) {
	t.Parallel()

	usage := skill.NewUsageStore()
	cfg := buildDefaultRunnerConfig(
		nil, nil, nil, nil,
		config.RuntimeConfig{MaxToolRounds: 3, LazyTools: true},
		nil, nil, nil, nil,
		usage,
		nil,
	)

	if cfg.SkillUsage == nil {
		t.Fatal("buildDefaultRunnerConfig().SkillUsage = nil, want the shared usage store")
	}
	if cfg.SkillUsage != agentruntime.SkillUsageRecorder(usage) {
		t.Fatalf("buildDefaultRunnerConfig().SkillUsage = %v, want the same store %v", cfg.SkillUsage, usage)
	}
	// Sanity: the other settings still flow through the extracted function
	// unchanged, so this refactor is not accidentally dropping fields.
	if cfg.MaxToolRounds != 3 {
		t.Errorf("buildDefaultRunnerConfig().MaxToolRounds = %d, want 3", cfg.MaxToolRounds)
	}
	if !cfg.LazyTools {
		t.Errorf("buildDefaultRunnerConfig().LazyTools = false, want true")
	}
}

// fakeEpisodeRecorder is a minimal agentruntime.EpisodeRecorder test double
// used only to prove the field flows through buildDefaultRunnerConfig; it
// records nothing and is never invoked by this test.
type fakeEpisodeRecorder struct{}

func (fakeEpisodeRecorder) RecordEpisode(domain.Agent, domain.Task, string, string) {}

// TestBuildDefaultRunnerConfigWiresEpisodeRecorder guards the default-runner
// half of the B3 review fix: the EpisodeRecorder constructed in
// BuildServeService (newEpisodeRecorder, wired into the resolver path via
// AgentRuntimeResolverConfig.EpisodeRecorder) must also reach
// defaultTaskRunner.runtimeCfg, or default-agent tasks — the GUI's primary
// path, since defaultCore.WithMemory shares the same episodicStore the
// recorder writes into — never record an episode despite the read side
// querying it on every later task's Prefetch.
func TestBuildDefaultRunnerConfigWiresEpisodeRecorder(t *testing.T) {
	t.Parallel()

	rec := fakeEpisodeRecorder{}
	cfg := buildDefaultRunnerConfig(
		nil, nil, nil, nil,
		config.RuntimeConfig{},
		nil, nil, nil, nil,
		nil,
		rec,
	)

	if cfg.EpisodeRecorder == nil {
		t.Fatal("buildDefaultRunnerConfig().EpisodeRecorder = nil, want the shared episode recorder")
	}
	if cfg.EpisodeRecorder != agentruntime.EpisodeRecorder(rec) {
		t.Fatalf("buildDefaultRunnerConfig().EpisodeRecorder = %v, want %v", cfg.EpisodeRecorder, rec)
	}
}
