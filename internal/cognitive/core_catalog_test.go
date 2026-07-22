package cognitive_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/domain"
)

// catalogStub is a capability.Provider whose entries are fixed, so a Core test
// can assert the full catalog reaches the prompt independent of the task.
type catalogStub struct{ entries []capability.Entry }

func (s catalogStub) Entries(context.Context) ([]capability.Entry, error) { return s.entries, nil }
func (s catalogStub) Detail(context.Context, string) (string, error) {
	return "", capability.ErrUnknownCapability
}

func TestBuildContextCarriesFullCatalogNotSelectedSkills(t *testing.T) {
	t.Parallel()
	catalog := capability.NewCatalog(catalogStub{entries: []capability.Entry{
		{Name: "go-testing", Group: "skills", Summary: "写 Go 测试", Kind: capability.KindSkill},
		{Name: "unrelated-skill", Group: "skills", Summary: "与任务无关", Kind: capability.KindSkill},
	}})
	core := cognitive.NewCore(cognitive.NoopCompressor{}).WithCatalog(catalog)

	built, err := core.BuildContext(context.Background(), cognitive.Request{
		Task: domain.Task{ID: "t1", Input: "写点 Go 测试"},
	})
	if err != nil {
		t.Fatalf("BuildContext() error = %v, want nil", err)
	}

	// 全量目录:与任务关键词无关的技能也必须在,否则又退回「系统替模型猜」。
	for _, want := range []string{"go-testing", "unrelated-skill"} {
		if !strings.Contains(built.Prompt, want) {
			t.Errorf("prompt missing %q; the catalog must list every skill, not a keyword-matched subset", want)
		}
	}
}

func TestBuildContextCatalogIsIdenticalAcrossTasks(t *testing.T) {
	t.Parallel()
	catalog := capability.NewCatalog(catalogStub{entries: []capability.Entry{
		{Name: "go-testing", Group: "skills", Summary: "写 Go 测试", Kind: capability.KindSkill},
	}})
	core := cognitive.NewCore(cognitive.NoopCompressor{}).WithCatalog(catalog)
	ctx := context.Background()

	first, err := core.BuildContext(ctx, cognitive.Request{Task: domain.Task{ID: "t1", Input: "写测试"}})
	if err != nil {
		t.Fatalf("first BuildContext() error = %v", err)
	}
	second, err := core.BuildContext(ctx, cognitive.Request{Task: domain.Task{ID: "t2", Input: "完全不同的输入"}})
	if err != nil {
		t.Fatalf("second BuildContext() error = %v", err)
	}

	firstCatalog := extractCatalogBlock(first.Prompt)
	secondCatalog := extractCatalogBlock(second.Prompt)
	if firstCatalog == "" {
		t.Fatalf("first prompt has no <available_capabilities> block:\n%s", first.Prompt)
	}
	// 目录进的是 prompt 缓存前缀。两个任务之间目录若不同,跨任务缓存必然 miss。
	if firstCatalog != secondCatalog {
		t.Errorf("catalog differs across tasks:\n%q\nvs\n%q", firstCatalog, secondCatalog)
	}
}

// erroringCatalogProvider fails when its entries are listed, so a test can
// assert BuildContext surfaces the catalog error rather than swallowing it.
type erroringCatalogProvider struct{ err error }

func (p erroringCatalogProvider) Entries(context.Context) ([]capability.Entry, error) {
	return nil, p.err
}
func (p erroringCatalogProvider) Detail(context.Context, string) (string, error) {
	return "", capability.ErrUnknownCapability
}

func TestBuildContextPropagatesCatalogError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("provider exploded")
	catalog := capability.NewCatalog(erroringCatalogProvider{err: sentinel})
	core := cognitive.NewCore(cognitive.NoopCompressor{}).WithCatalog(catalog)

	_, err := core.BuildContext(context.Background(), cognitive.Request{
		Task: domain.Task{ID: "t1", Input: "anything"},
	})
	if err == nil {
		t.Fatal("BuildContext() error = nil, want the catalog error surfaced, not swallowed")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("BuildContext() error = %v, want it to wrap the provider error", err)
	}
}

func extractCatalogBlock(prompt string) string {
	start := strings.Index(prompt, "<available_capabilities>")
	end := strings.Index(prompt, "</available_capabilities>")
	if start < 0 || end < 0 {
		return ""
	}
	return prompt[start : end+len("</available_capabilities>")]
}
