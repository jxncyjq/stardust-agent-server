package capability_test

import (
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
)

func sampleEntries() []capability.Entry {
	return []capability.Entry{
		{Name: "read_file", Group: "files", Summary: "Read a file", Kind: capability.KindTool},
		{Name: "write_file", Group: "files", Summary: "Write a file", Kind: capability.KindTool},
		{Name: "go-testing", Group: "skills", Summary: "写 Go 测试", Kind: capability.KindSkill},
	}
}

func TestRenderIsByteStable(t *testing.T) {
	t.Parallel()
	// 目录进 prompt 的缓存前缀(runtime.go:309)。渲染结果只要有一个字节
	// 不稳定,provider 侧的 prompt 缓存就每轮 miss。
	first := capability.Render(sampleEntries())
	second := capability.Render(sampleEntries())
	if first != second {
		t.Fatalf("Render() is not byte-stable:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestRenderGroupsEntriesUnderHeadings(t *testing.T) {
	t.Parallel()
	got := capability.Render(sampleEntries())

	for _, want := range []string{
		"<available_capabilities>",
		"files:",
		"  - read_file: Read a file",
		"skills:",
		"  - go-testing: 写 Go 测试",
		"</available_capabilities>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Render() missing %q, got:\n%s", want, got)
		}
	}
	if strings.Index(got, "files:") > strings.Index(got, "skills:") {
		t.Error("groups are out of order: rendering must follow the sorted entries")
	}
}

func TestRenderEmptyCatalogRendersNothing(t *testing.T) {
	t.Parallel()
	// 空目录不该在 prompt 里留一个空壳块 —— 那只会让模型以为自己有能力可用。
	if got := capability.Render(nil); got != "" {
		t.Errorf("Render(nil) = %q, want empty", got)
	}
}

func TestRenderCarriesLoadInstruction(t *testing.T) {
	t.Parallel()
	got := capability.Render(sampleEntries())
	if !strings.Contains(got, "load_capabilities") {
		t.Error("rendered catalog does not tell the model how to load anything: a listing with no instruction is the claw-code failure mode")
	}
}
