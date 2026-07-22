package capability_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/skill"
)

// writeSkill lays down a minimal SKILL.md that internal/skill's readSkill can
// parse. Skill.Summary comes from the front matter's "summary" key,
// independent of Skill.Content (the body after the closing "---") -- see
// internal/skill/system.go's readSkill.
//
// When summary is non-empty, the front matter carries a "summary:" line and
// the body carries a "正文内容 <id>" marker used to prove Detail() returns the
// full body, distinct from the catalog summary. When summary is empty, the
// front matter omits the "summary" key entirely -- the legal way, per
// readSkill's contract, to produce a skill with no summary.
func writeSkill(t *testing.T, root, id, summary string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", dir, err)
	}
	var body string
	if summary == "" {
		body = fmt.Sprintf("---\nid: %s\nname: %s\nstatus: active\n---\n\n正文内容 %s\n", id, id, id)
	} else {
		body = fmt.Sprintf("---\nid: %s\nname: %s\nsummary: %s\nstatus: active\n---\n\n正文内容 %s\n", id, id, summary, id)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
}

func newSkillSystem(t *testing.T, root string) *skill.System {
	t.Helper()
	return skill.NewSystem(skill.Config{Roots: []string{root}})
}

func TestSkillProviderEntriesUseSummaryNotContent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkill(t, root, "go-testing", "写 Go 表驱动测试")

	entries, err := capability.NewSkillProvider(newSkillSystem(t, root)).Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries() error = %v, want nil", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Entries() len = %d, want 1", len(entries))
	}
	if entries[0].Summary != "写 Go 表驱动测试" {
		t.Errorf("summary = %q, want the first line of the body", entries[0].Summary)
	}
	if strings.Contains(entries[0].Summary, "正文内容") {
		t.Error("summary contains the skill body: the catalog must never carry full content")
	}
	if entries[0].Kind != capability.KindSkill {
		t.Errorf("kind = %v, want KindSkill", entries[0].Kind)
	}
}

func TestSkillProviderRejectsSkillWithoutSummary(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkill(t, root, "no-summary", "")

	// 旧实现在 Summary 为空时会退回注入整篇正文(cognitive/core.go:255)。
	// 目录里没有这条退路:没有一行说明的技能无法被判断,必须让作者补上。
	_, err := capability.NewSkillProvider(newSkillSystem(t, root)).Entries(context.Background())
	if err == nil {
		t.Fatal("Entries() error = nil, want an error naming the skill without a summary")
	}
	if !strings.Contains(err.Error(), "no-summary") {
		t.Errorf("error = %q, want it to name the offending skill", err)
	}
}

func TestSkillProviderRejectsTooManySkills(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for i := 0; i <= capability.MaxSkillsPerAgent; i++ {
		writeSkill(t, root, fmt.Sprintf("skill-%03d", i), "一行说明")
	}

	_, err := capability.NewSkillProvider(newSkillSystem(t, root)).Entries(context.Background())
	if err == nil {
		t.Fatalf("Entries() error = nil, want an error at %d skills", capability.MaxSkillsPerAgent+1)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(capability.MaxSkillsPerAgent)) {
		t.Errorf("error = %q, want it to state the limit", err)
	}
}

func TestSkillProviderDetailReturnsBody(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkill(t, root, "go-testing", "写 Go 表驱动测试")

	detail, err := capability.NewSkillProvider(newSkillSystem(t, root)).Detail(context.Background(), "go-testing")
	if err != nil {
		t.Fatalf("Detail() error = %v, want nil", err)
	}
	if !strings.Contains(detail, "正文内容 go-testing") {
		t.Errorf("detail = %q, want it to carry the skill body", detail)
	}
}

func TestSkillProviderDetailUnknownName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	_, err := capability.NewSkillProvider(newSkillSystem(t, root)).Detail(context.Background(), "nope")
	if !errors.Is(err, capability.ErrUnknownCapability) {
		t.Fatalf("Detail(nope) error = %v, want ErrUnknownCapability", err)
	}
}

func TestSkillProviderNilSystemFailsLoud(t *testing.T) {
	t.Parallel()

	// Task 1's ToolProvider treats a nil dependency as an assembly bug, not a
	// legitimate empty state (see ErrNilRegistry in tool_provider.go), and a
	// prior nil-registry check in this package was flagged Critical for
	// returning (nil, nil) instead of erroring. SkillProvider must not repeat
	// that: a nil *skill.System means the provider was never wired up, and
	// pretending it has zero skills would hide that assembly bug.
	provider := capability.NewSkillProvider(nil)
	ctx := context.Background()

	if _, err := provider.Entries(ctx); err == nil {
		t.Fatal("Entries() error = nil, want an error for a nil skill system")
	}
	if _, err := provider.Detail(ctx, "anything"); err == nil {
		t.Fatal("Detail() error = nil, want an error for a nil skill system")
	}
}

func TestSkillProviderCacheInvalidatesWhenSkillChanges(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkill(t, root, "one", "第一版说明")
	provider := capability.NewSkillProvider(newSkillSystem(t, root))
	ctx := context.Background()

	if _, err := provider.Entries(ctx); err != nil {
		t.Fatalf("first Entries() error = %v", err)
	}
	writeSkill(t, root, "two", "第二版说明")

	entries, err := provider.Entries(ctx)
	if err != nil {
		t.Fatalf("second Entries() error = %v", err)
	}
	// 缓存过期未被识别,新技能就永远不出现在目录里 —— 静默不可发现。
	if len(entries) != 2 {
		t.Fatalf("Entries() len = %d, want 2 after a new skill appeared", len(entries))
	}
}
