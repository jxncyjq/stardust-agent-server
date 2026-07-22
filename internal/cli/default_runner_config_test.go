package cli

import (
	"testing"

	"github.com/stardust/legion-agent/internal/config"
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
