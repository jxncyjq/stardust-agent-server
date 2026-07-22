package capability_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
)

// fakeProvider 是一个可控的 Provider 桩，用于测试聚合语义本身，
// 不牵扯 tool.Registry / skill.System 的真实行为。
type fakeProvider struct {
	entries []capability.Entry
	details map[string]string
	err     error
}

func (p fakeProvider) Entries(context.Context) ([]capability.Entry, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.entries, nil
}

func (p fakeProvider) Detail(_ context.Context, name string) (string, error) {
	detail, ok := p.details[name]
	if !ok {
		return "", capability.ErrUnknownCapability
	}
	return detail, nil
}

func TestCatalogEntriesSortsByGroupThenName(t *testing.T) {
	t.Parallel()
	catalog := capability.NewCatalog(
		fakeProvider{entries: []capability.Entry{
			{Name: "write_file", Group: "files", Summary: "写文件", Kind: capability.KindTool},
			{Name: "read_file", Group: "files", Summary: "读文件", Kind: capability.KindTool},
		}},
		fakeProvider{entries: []capability.Entry{
			{Name: "go-testing", Group: "skills", Summary: "写 Go 测试", Kind: capability.KindSkill},
		}},
	)

	entries, err := catalog.Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries() error = %v, want nil", err)
	}

	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Group+"/"+e.Name)
	}
	want := []string{"files/read_file", "files/write_file", "skills/go-testing"}
	if len(got) != len(want) {
		t.Fatalf("Entries() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Entries()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCatalogEntriesRejectsDuplicateName(t *testing.T) {
	t.Parallel()
	catalog := capability.NewCatalog(
		fakeProvider{entries: []capability.Entry{
			{Name: "clash", Group: "files", Summary: "一号", Kind: capability.KindTool},
		}},
		fakeProvider{entries: []capability.Entry{
			{Name: "clash", Group: "skills", Summary: "二号", Kind: capability.KindSkill},
		}},
	)

	// 同名条目会让 load/call 无法确定指向谁，属于装配错误,必须报错而不是任选一个。
	_, err := catalog.Entries(context.Background())
	if err == nil {
		t.Fatal("Entries() error = nil, want an error naming the duplicated capability")
	}
	if !strings.Contains(err.Error(), "clash") {
		t.Errorf("Entries() error = %q, want it to name the duplicate %q", err, "clash")
	}
}

func TestCatalogEntriesPropagatesProviderFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("skills root unreadable")
	catalog := capability.NewCatalog(fakeProvider{err: boom})

	_, err := catalog.Entries(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Entries() error = %v, want it to wrap %v", err, boom)
	}
}

func TestCatalogEntriesRejectsInvalidEntry(t *testing.T) {
	t.Parallel()
	long := make([]rune, capability.MaxSummaryChars+1)
	for i := range long {
		long[i] = 'x'
	}
	cases := map[string]capability.Entry{
		"missing summary":  {Name: "a", Group: "files"},
		"missing group":    {Name: "a", Summary: "有说明"},
		"missing name":     {Group: "files", Summary: "有说明"},
		"summary too long": {Name: "a", Group: "files", Summary: string(long)},
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			catalog := capability.NewCatalog(fakeProvider{entries: []capability.Entry{entry}})
			if _, err := catalog.Entries(context.Background()); err == nil {
				t.Fatalf("Entries() error = nil, want an error for %s", name)
			}
		})
	}
}

func TestCatalogDetailUnknownName(t *testing.T) {
	t.Parallel()
	catalog := capability.NewCatalog(fakeProvider{
		entries: []capability.Entry{{Name: "a", Group: "files", Summary: "s"}},
		details: map[string]string{"a": "全文"},
	})

	if _, err := catalog.Detail(context.Background(), "nope"); !errors.Is(err, capability.ErrUnknownCapability) {
		t.Fatalf("Detail(nope) error = %v, want ErrUnknownCapability", err)
	}
}

func TestCatalogDetailReturnsProviderContent(t *testing.T) {
	t.Parallel()
	catalog := capability.NewCatalog(fakeProvider{
		entries: []capability.Entry{{Name: "a", Group: "files", Summary: "s"}},
		details: map[string]string{"a": "全文内容"},
	})

	got, err := catalog.Detail(context.Background(), "a")
	if err != nil {
		t.Fatalf("Detail(a) error = %v, want nil", err)
	}
	if got != "全文内容" {
		t.Errorf("Detail(a) = %q, want %q", got, "全文内容")
	}
}
