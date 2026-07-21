# 能力目录与按需加载 实施计划（第一部分：技能侧闭环）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把技能从「按关键词猜 top 3、Summary 为空注入全文」改为「全量目录进 system prompt + 模型按需 `load_capabilities` 拉正文」，并建立工具将来复用的同一套目录抽象。

**Architecture:** 新增只读聚合包 `internal/capability`，用 `Provider` 抽象把 `tool.Registry` 与 `skill.System` 统一成「条目（名字+分组+一行）+ 明细按需取」两个动作；目录渲染进 `basePrompt`（即 prompt 缓存的稳定前缀），已加载的明细进 run state 的独立区块并随 checkpoint 持久化。执行路径完全不变 —— `capability` 不执行任何东西。

**Tech Stack:** Go 1.26，标准库；测试用 `testing` + 表驱动；无新增第三方依赖。

设计文档：`docs/superpowers/specs/2026-07-21-capability-catalog-design.md`

**本计划不含**（另出计划）：阈值门控、`search_capabilities`/`call_tool` 的按条件注册、删除 `lazy_tools` 配置项。本计划中 `call_tool` 与现有 lazy 协议维持原状。

## Global Constraints

- **Fail-loud 铁律**（`CLAUDE.md` 第 0 节，覆盖一切便利写法）：禁止返回零值假装正常、禁止丢弃 error、禁止静默跳过、禁止 `default:`/`else` 吞掉非预期状态。唯一豁免是契约显式声明的「可选」。
- 公开 API 必须有 Go doc 风格注释（以标识符名开头）。
- 错误传播用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装。
- 门禁：`gofmt -l .` 为空、`go build ./... && go vet ./... && go test ./...` 全绿。
- 受影响包需跑 `-race`：Windows 用 `PATH="/c/Users/Administrator/AppData/Local/Microsoft/WinGet/Packages/MartinStorsjo.LLVM-MinGW.MSVCRT_Microsoft.Winget.Source_8wekyb3d8bbwe/llvm-mingw-20260616-msvcrt-x86_64/bin:$PATH" CGO_ENABLED=1 go test -race ./...`
- **变异测试是本仓库惯例**：每个任务的测试写完并通过后，把生产代码改回旧写法，确认测试**确实失败**，再改回来。这一步不能省。
- 每个任务结束时提交一次，提交信息用中文，正文说明「为什么」而非「改了什么」。
- 常量：`MaxSummaryChars = 120`、`MaxSkillsPerAgent = 64`、`MaxLoadBatch = 5`。

## File Structure

| 文件 | 责任 |
|---|---|
| `internal/capability/catalog.go`（新） | `Kind` / `Entry` / `Provider` / `Catalog`：聚合、校验、去重、确定序 |
| `internal/capability/render.go`（新） | 目录渲染成 prompt 文本，逐字节稳定 |
| `internal/capability/tool_provider.go`（新） | 包 `*tool.Registry`，`Detail` 输出 schema JSON |
| `internal/capability/skill_provider.go`（新） | 包 `*skill.System`，含 64 上限与 mtime 缓存 |
| `internal/tool/descriptor.go`（改） | `Descriptor` 新增 `Group` 字段 |
| `internal/tool/builtin.go` 等（改） | 15 个内置工具标注 `Group` |
| `internal/runtime/runtime.go`（改） | run state 加 `loaded`；prompt 三段式与预算；checkpoint 存取 |
| `internal/runtime/lazytools.go`（改） | `list_tools` 换成 `load_capabilities`；目录渲染移出 |
| `internal/runtime/delegation.go`（改） | `newSubRuntime` 显式给空 `loaded` |
| `internal/cognitive/core.go`（改） | `skillBlock` 从「选 top 3 注入」改为「渲染目录」 |

---

### Task 1: `capability` 包骨架与聚合语义

**Files:**
- Create: `internal/capability/catalog.go`
- Test: `internal/capability/catalog_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `type Kind uint8`，常量 `KindTool` / `KindSkill`，方法 `String() string`
  - `type Entry struct { Name, Group, Summary string; Kind Kind }`，方法 `Validate() error`
  - `const MaxSummaryChars = 120`
  - `type Provider interface { Entries(ctx context.Context) ([]Entry, error); Detail(ctx context.Context, name string) (string, error) }`
  - `type Catalog struct{ ... }`，`func NewCatalog(providers ...Provider) *Catalog`
  - `func (c *Catalog) Entries(ctx context.Context) ([]Entry, error)`
  - `func (c *Catalog) Detail(ctx context.Context, name string) (string, error)`
  - `var ErrUnknownCapability = errors.New("unknown capability")`

- [ ] **Step 1: 写失败测试**

创建 `internal/capability/catalog_test.go`：

```go
package capability_test

import (
	"context"
	"errors"
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
	if !contains(err.Error(), "clash") {
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
		"missing summary": {Name: "a", Group: "files"},
		"missing group":   {Name: "a", Summary: "有说明"},
		"missing name":    {Group: "files", Summary: "有说明"},
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

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/capability/ -run TestCatalog -count=1`
Expected: FAIL，编译错误 `no required module provides package .../internal/capability`（包尚不存在）

- [ ] **Step 3: 写最小实现**

创建 `internal/capability/catalog.go`：

```go
// Package capability aggregates the agent's callable tools and its loadable
// skills into one read-only directory: each entry is a name, a group and a
// one-line summary, and each entry's full definition can be fetched on demand.
//
// It deliberately does not execute anything. Tool calls keep going through
// tool.Registry so permission, audit, timeout and the manual-approval gate all
// stay on the one path they were written for.
package capability

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ErrUnknownCapability reports a name that no provider offers. Callers turn it
// into a message for the model rather than aborting the task.
var ErrUnknownCapability = errors.New("unknown capability")

// MaxSummaryChars bounds a catalog entry's one-line summary. The catalog sits
// in the prompt's cached prefix and is re-sent every round, so a runaway
// summary is paid for on every inference.
const MaxSummaryChars = 120

// Kind distinguishes what an entry is, because the two behave differently once
// loaded: a tool is invoked, a skill is read.
type Kind uint8

const (
	KindTool Kind = iota
	KindSkill
)

// String returns the lowercase name used in rendered catalogs.
func (k Kind) String() string {
	switch k {
	case KindTool:
		return "tool"
	case KindSkill:
		return "skill"
	default:
		return fmt.Sprintf("kind(%d)", uint8(k))
	}
}

// Entry is one line of the catalog.
type Entry struct {
	Name    string
	Group   string
	Summary string
	Kind    Kind
}

// Validate reports why an entry may not enter the catalog. Every field is
// author-controlled, so a violation is a fixable mistake in a tool descriptor
// or a skill's front matter -- reported, never trimmed away.
func (e Entry) Validate() error {
	if e.Name == "" {
		return errors.New("capability entry: name is empty")
	}
	if e.Group == "" {
		return fmt.Errorf("capability %q: group is empty", e.Name)
	}
	if e.Summary == "" {
		return fmt.Errorf("capability %q: summary is empty", e.Name)
	}
	if n := len([]rune(e.Summary)); n > MaxSummaryChars {
		return fmt.Errorf("capability %q: summary is %d chars, limit %d", e.Name, n, MaxSummaryChars)
	}
	return nil
}

// Provider supplies one class of capability.
type Provider interface {
	// Entries lists this provider's catalog lines.
	Entries(ctx context.Context) ([]Entry, error)
	// Detail returns the full definition of one capability: a tool's JSON
	// schema, or a skill's body. It returns ErrUnknownCapability for a name it
	// does not offer.
	Detail(ctx context.Context, name string) (string, error)
}

// Catalog is the aggregated, validated, deterministically ordered directory.
type Catalog struct {
	providers []Provider
}

// NewCatalog returns a Catalog over the given providers. Provider order does
// not affect output: entries are sorted by group then name.
func NewCatalog(providers ...Provider) *Catalog {
	return &Catalog{providers: providers}
}

// Entries returns every provider's entries, validated, checked for duplicate
// names, and sorted by (group, name).
//
// The ordering is not cosmetic: the rendered catalog goes into the prompt's
// cached prefix, so any instability in it costs a cache miss on every round.
func (c *Catalog) Entries(ctx context.Context) ([]Entry, error) {
	all := make([]Entry, 0)
	seen := make(map[string]string)
	for _, provider := range c.providers {
		entries, err := provider.Entries(ctx)
		if err != nil {
			return nil, fmt.Errorf("list capabilities: %w", err)
		}
		for _, entry := range entries {
			if err := entry.Validate(); err != nil {
				return nil, err
			}
			if group, ok := seen[entry.Name]; ok {
				return nil, fmt.Errorf("capability %q declared twice (groups %q and %q): a duplicate name has no single meaning for load or call", entry.Name, group, entry.Group)
			}
			seen[entry.Name] = entry.Group
			all = append(all, entry)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Group == all[j].Group {
			return all[i].Name < all[j].Name
		}
		return all[i].Group < all[j].Group
	})
	return all, nil
}

// Detail returns one capability's full definition, or ErrUnknownCapability.
func (c *Catalog) Detail(ctx context.Context, name string) (string, error) {
	for _, provider := range c.providers {
		detail, err := provider.Detail(ctx, name)
		if errors.Is(err, ErrUnknownCapability) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("load capability %q: %w", name, err)
		}
		return detail, nil
	}
	return "", fmt.Errorf("%w: %s", ErrUnknownCapability, name)
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/capability/ -count=1`
Expected: `ok  github.com/stardust/legion-agent/internal/capability`

- [ ] **Step 5: 变异测试**

把 `Entries` 的 `sort.Slice` 整段注释掉，跑 `go test ./internal/capability/ -run TestCatalogEntriesSortsByGroupThenName -count=1`。
Expected: FAIL（顺序断言失败）。确认后改回。

再把重复名检查（`if group, ok := seen[entry.Name]; ok` 那一段）注释掉，跑 `-run TestCatalogEntriesRejectsDuplicateName`。
Expected: FAIL。确认后改回。

- [ ] **Step 6: 提交**

```bash
git add internal/capability/
git commit -m "feat(capability): 能力目录的聚合、校验与确定序

新增只读聚合层,把工具与技能统一成「条目 + 明细按需取」两个动作。

排序不是装饰:渲染后的目录会进 prompt 的缓存前缀,顺序不稳定等于每轮
都 miss 一次缓存,所以按 (group, name) 强制确定序,不依赖 map 迭代序。

同名条目直接报错而不是任选其一 —— 重名之后 load 与 call 都无法确定
指向谁,属于装配错误。条目字段全部由作者可控,校验失败一律报错不截断。"
```

---

### Task 2: `Descriptor.Group` 与 `ToolProvider`

> **实施后修正（已合入，见提交 `422f968`）**：本任务 Step 3 给出的参考实现有两处被审查否掉，实际代码与下文不同，以代码为准 ——
> (1) `registry == nil` 时不再返回 `nil, nil`，改为 fail-loud 报错并有测试覆盖（返回空目录会让「确实没工具」与「装配错了」不可区分）；
> (2) `summarize()` 不再按 `.` / `。` / `
` 切句 —— 工具描述里插了工作区根路径，而 per-agent 的会话根形如 `<working_dir>/.stardust` 含点，原写法会把摘要截在路径中间。

**Files:**
- Modify: `internal/tool/descriptor.go:9-18`（`Descriptor` 结构体）
- Modify: `internal/tool/builtin.go`、`internal/tool/taskledger.go`、`internal/tool/agent_message.go`、`internal/tool/web.go`、`internal/tool/session_search.go`、`internal/runtime/delegation_tool.go`、`internal/runtime/moa_tool.go`（标注 `Group`）
- Create: `internal/capability/tool_provider.go`
- Test: `internal/capability/tool_provider_test.go`

**Interfaces:**
- Consumes: Task 1 的 `Entry` / `Provider` / `ErrUnknownCapability`
- Produces:
  - `tool.Descriptor` 新增字段 `Group string`
  - `func NewToolProvider(registry *tool.Registry) *ToolProvider`
  - `ToolProvider` 实现 `capability.Provider`；`Detail` 返回 `{"name":...,"description":...,"input_schema":...}` 的 JSON

- [ ] **Step 1: 写失败测试**

创建 `internal/capability/tool_provider_test.go`：

```go
package capability_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/tool"
)

func newTestRegistry(t *testing.T, descriptors ...tool.Descriptor) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry(nil, nil, tool.Guardrails{})
	for _, descriptor := range descriptors {
		registry.RegisterDescriptor(descriptor, tool.HandlerFunc(
			func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
				return domain.ToolResult{}, nil
			}))
	}
	return registry
}

func TestToolProviderEntriesCarryGroupAndSummary(t *testing.T) {
	t.Parallel()
	registry := newTestRegistry(t, tool.Descriptor{
		Name:        "read_file",
		Group:       "files",
		Description: "Read a UTF-8 text file inside the workspace root.",
		InputSchema: map[string]any{"type": "object"},
	})

	entries, err := capability.NewToolProvider(registry).Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries() error = %v, want nil", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Entries() len = %d, want 1", len(entries))
	}
	if entries[0].Group != "files" {
		t.Errorf("group = %q, want %q", entries[0].Group, "files")
	}
	if entries[0].Kind != capability.KindTool {
		t.Errorf("kind = %v, want KindTool", entries[0].Kind)
	}
	if entries[0].Summary == "" {
		t.Error("summary is empty, want the descriptor's first sentence")
	}
}

func TestToolProviderDetailIsTheSchemaTheModelWouldHaveSeen(t *testing.T) {
	t.Parallel()
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
	}
	registry := newTestRegistry(t, tool.Descriptor{
		Name:        "read_file",
		Group:       "files",
		Description: "Read a file.",
		InputSchema: schema,
	})

	detail, err := capability.NewToolProvider(registry).Detail(context.Background(), "read_file")
	if err != nil {
		t.Fatalf("Detail(read_file) error = %v, want nil", err)
	}
	var decoded struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"input_schema"`
	}
	if err := json.Unmarshal([]byte(detail), &decoded); err != nil {
		t.Fatalf("Detail is not valid JSON: %v (%s)", err, detail)
	}
	if decoded.Name != "read_file" {
		t.Errorf("name = %q, want %q", decoded.Name, "read_file")
	}
	if decoded.InputSchema == nil {
		t.Error("input_schema missing: the model cannot call a tool whose parameters it never sees")
	}
}

func TestToolProviderDetailUnknownName(t *testing.T) {
	t.Parallel()
	registry := newTestRegistry(t)

	_, err := capability.NewToolProvider(registry).Detail(context.Background(), "nope")
	if !errors.Is(err, capability.ErrUnknownCapability) {
		t.Fatalf("Detail(nope) error = %v, want ErrUnknownCapability", err)
	}
}

func TestToolProviderRejectsDescriptorWithoutGroup(t *testing.T) {
	t.Parallel()
	registry := newTestRegistry(t, tool.Descriptor{
		Name:        "ungrouped",
		Description: "No group declared.",
		InputSchema: map[string]any{"type": "object"},
	})

	// 未标注分组的工具无法在目录里落位,这是注册时的疏漏,必须报出来。
	if _, err := capability.NewToolProvider(registry).Entries(context.Background()); err == nil {
		t.Fatal("Entries() error = nil, want an error naming the ungrouped tool")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/capability/ -run TestToolProvider -count=1`
Expected: FAIL，编译错误 `unknown field Group in struct literal of type tool.Descriptor` 与 `undefined: capability.NewToolProvider`

- [ ] **Step 3: 写最小实现**

先改 `internal/tool/descriptor.go`，在 `Descriptor` 里加字段：

```go
type Descriptor struct {
	Name        string
	Description string
	InputSchema map[string]any
	RiskLevel   string
	Timeout     time.Duration
	// Group places this tool in the capability catalog, by what it is for
	// ("files", "tasks", "messages"), not by where it came from. It is what a
	// model scanning the catalog reads first, so it is required: an unplaced
	// tool cannot be listed.
	Group string
	// Sensitive 标记一个工具为有副作用：Manual 模式下对它的调用被挡在人工审批后
	// （M2b），Plan 模式把它排除出所提供的工具集。只读工具（read/search/list）非敏感。
	Sensitive bool `json:"sensitive,omitempty"`
}
```

再创建 `internal/capability/tool_provider.go`：

```go
package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/tool"
)

// ToolProvider exposes a tool registry as catalog entries.
//
// It reads whatever registry it is handed rather than a global one, because
// the registry a task may use is already narrowed (Plan mode drops the
// side-effecting tools, per-agent tasks get their own set). A catalog built
// from a wider registry than the caller's would advertise tools that task is
// not allowed to run.
type ToolProvider struct {
	registry *tool.Registry
}

// NewToolProvider returns a Provider backed by registry.
func NewToolProvider(registry *tool.Registry) *ToolProvider {
	return &ToolProvider{registry: registry}
}

// Entries lists the registry's tools as catalog lines.
func (p *ToolProvider) Entries(context.Context) ([]Entry, error) {
	if p.registry == nil {
		return nil, nil
	}
	descriptors := p.registry.Descriptors()
	entries := make([]Entry, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.Group == "" {
			return nil, fmt.Errorf("tool %q declares no catalog group", descriptor.Name)
		}
		entries = append(entries, Entry{
			Name:    descriptor.Name,
			Group:   descriptor.Group,
			Summary: summarize(descriptor.Description),
			Kind:    KindTool,
		})
	}
	return entries, nil
}

// Detail returns the tool's name, description and input schema as JSON -- the
// same three fields the model would have received had the tool been offered
// natively.
func (p *ToolProvider) Detail(_ context.Context, name string) (string, error) {
	if p.registry == nil {
		return "", fmt.Errorf("%w: %s", ErrUnknownCapability, name)
	}
	for _, descriptor := range p.registry.Descriptors() {
		if descriptor.Name != name {
			continue
		}
		encoded, err := json.Marshal(map[string]any{
			"name":         descriptor.Name,
			"description":  descriptor.Description,
			"input_schema": descriptor.InputSchema,
		})
		if err != nil {
			return "", fmt.Errorf("marshal tool %q schema: %w", name, err)
		}
		return string(encoded), nil
	}
	return "", fmt.Errorf("%w: %s", ErrUnknownCapability, name)
}

// summarize reduces a tool description to its first sentence, bounded by
// MaxSummaryChars. Tool descriptions are written for the model that is about
// to call the tool and run long; the catalog only needs enough to decide
// whether to load the whole thing.
func summarize(description string) string {
	text := strings.TrimSpace(description)
	if idx := strings.IndexAny(text, ".。\n"); idx > 0 {
		text = strings.TrimSpace(text[:idx])
	}
	runes := []rune(text)
	if len(runes) > MaxSummaryChars {
		return string(runes[:MaxSummaryChars])
	}
	return text
}
```

- [ ] **Step 4: 标注 15 个内置工具的 Group**

在每个描述符字面量里加 `Group:` 字段，取值如下（**不要改任何其他字段**）：

| 文件 | 工具 | Group |
|---|---|---|
| `internal/tool/builtin.go` | `read_file` `write_file` `search_content` `list_files` | `files` |
| `internal/tool/taskledger.go` | `create_task` `claim_task` `update_task` `append_task_message` `read_task` `rebuild_tasks` | `tasks` |
| `internal/tool/agent_message.go` | `send_message` `read_messages` | `messages` |
| `internal/tool/web.go` | `fetch_url` | `web` |
| `internal/tool/session_search.go` | `session_search` | `history` |
| `internal/runtime/delegation_tool.go` | `delegate_task` | `agents` |
| `internal/runtime/moa_tool.go` | `moa_consult` | `agents` |

`taskledger.go` 与 `agent_message.go` 走的是共用构造函数（`taskDescriptor` / `messageDescriptor`），在那两个函数里加一个 `group` 参数并由各调用点传入，不要逐个复制。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/capability/ ./internal/tool/ ./internal/runtime/ -count=1`
Expected: 三个包全部 `ok`

- [ ] **Step 6: 变异测试**

把 `Entries` 里 `if descriptor.Group == ""` 那段删掉，跑 `-run TestToolProviderRejectsDescriptorWithoutGroup`。
Expected: FAIL。确认后改回。

- [ ] **Step 7: 提交**

```bash
git add internal/tool/ internal/capability/ internal/runtime/delegation_tool.go internal/runtime/moa_tool.go
git commit -m "feat(tool,capability): 工具按能力域分组并接入能力目录

Descriptor 新增 Group,按「这工具是干什么的」分组而不是「它从哪来」——
模型在目录里找工具时想的是前者。未标注分组的工具会被 ToolProvider 报错
拒绝入目录,因为无处落位的条目等于不可发现。

ToolProvider 读调用方传入的 registry 而非全局的:任务可用的工具集本来就
是收窄过的(Plan 模式去掉有副作用的工具、per-agent 各有各的集合),用更宽
的 registry 建目录会广告出该任务无权运行的工具。

目录条目只取描述的第一句并限长,工具描述是写给「即将调用它的模型」的、
往往很长,目录只需要够判断「要不要把它整个拉进来」。"
```

---

### Task 3: `SkillProvider`（64 上限 + mtime 缓存）

**Files:**
- Create: `internal/capability/skill_provider.go`
- Test: `internal/capability/skill_provider_test.go`

**Interfaces:**
- Consumes: Task 1 的 `Entry` / `Provider`；`skill.System`（`internal/skill/system.go:108`）的 `Load(ctx) ([]Skill, error)`
- Produces:
  - `const MaxSkillsPerAgent = 64`
  - `func NewSkillProvider(system *skill.System) *SkillProvider`
  - `SkillProvider` 实现 `capability.Provider`；`Detail` 返回技能 `Content`

**注意**：`skill.Skill`（`internal/skill/system.go:87`）的字段是 `ID / Name / Summary / Content / Tags / Status / RiskLevel`，**没有 Group 字段**。本期所有技能统一归入 `skills` 组。`Entries` 只收 `skill.IsInjectable` 为真的技能（`internal/skill/system.go:325`），与既有注入路径的口径一致。

- [ ] **Step 1: 写失败测试**

创建 `internal/capability/skill_provider_test.go`：

```go
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

// writeSkill 落一个最小可读的技能文件。字段名以 internal/skill/system.go 的
// readSkill 解析口径为准；若解析失败测试会在 Load 处直接暴露。
func writeSkill(t *testing.T, root, id, summary string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", dir, err)
	}
	body := fmt.Sprintf("---\nid: %s\nname: %s\nsummary: %s\nstatus: active\n---\n\n正文内容 %s\n", id, id, summary, id)
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
		t.Errorf("summary = %q, want the front-matter summary", entries[0].Summary)
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/capability/ -run TestSkillProvider -count=1`
Expected: FAIL，`undefined: capability.NewSkillProvider`

**若 `skill.Config` 的字段名不是 `Roots`、或 front matter 键名与测试里写的不符**：先读 `internal/skill/system.go` 的 `NewSystem` 与 `readSkill`（约 `system.go:115` 与 `system.go:242`）确认真实字段名，改测试里的构造与文件内容，**不要改生产代码去迁就测试**。

- [ ] **Step 3: 写最小实现**

创建 `internal/capability/skill_provider.go`：

```go
package capability

import (
	"context"
	"fmt"
	"sync"

	"github.com/stardust/legion-agent/internal/skill"
)

// MaxSkillsPerAgent bounds how many skills one agent may expose.
//
// The catalog is rendered into the prompt's cached prefix on every inference,
// so its size is a standing cost. The limit is a declared contract, not a
// suggestion: exceeding it fails loudly, because silently dropping the tail
// would leave those skills listed nowhere and therefore unreachable.
const MaxSkillsPerAgent = 64

// SkillProvider exposes an agent's skills as catalog entries.
type SkillProvider struct {
	system *skill.System

	mu     sync.Mutex
	cached []Entry
	bodies map[string]string
	loaded bool
}

// NewSkillProvider returns a Provider backed by system.
func NewSkillProvider(system *skill.System) *SkillProvider {
	return &SkillProvider{system: system}
}

// Entries lists the agent's injectable skills, one line each.
func (p *SkillProvider) Entries(ctx context.Context) ([]Entry, error) {
	if err := p.refresh(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Entry, len(p.cached))
	copy(out, p.cached)
	return out, nil
}

// Detail returns a skill's full body.
func (p *SkillProvider) Detail(ctx context.Context, name string) (string, error) {
	if err := p.refresh(ctx); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	body, ok := p.bodies[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownCapability, name)
	}
	return body, nil
}

// refresh reloads the skill set. skill.System.Load already walks the roots on
// every call, so this keeps only the derived catalog rather than re-deriving
// entries per inference round.
func (p *SkillProvider) refresh(ctx context.Context) error {
	if p.system == nil {
		return nil
	}
	skills, err := p.system.Load(ctx)
	if err != nil {
		return fmt.Errorf("load skills: %w", err)
	}
	entries := make([]Entry, 0, len(skills))
	bodies := make(map[string]string, len(skills))
	for _, s := range skills {
		if !skill.IsInjectable(s) {
			continue
		}
		if s.Summary == "" {
			return fmt.Errorf("skill %q at %q declares no summary: a catalog line cannot be derived from its body", s.ID, s.Path)
		}
		entries = append(entries, Entry{
			Name:    s.ID,
			Group:   "skills",
			Summary: s.Summary,
			Kind:    KindSkill,
		})
		bodies[s.ID] = s.Content
	}
	if len(entries) > MaxSkillsPerAgent {
		return fmt.Errorf("agent exposes %d skills, limit %d: trim the skills directory or split it across agents", len(entries), MaxSkillsPerAgent)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cached = entries
	p.bodies = bodies
	p.loaded = true
	return nil
}
```

**关于缓存**：`skill.System.Load` 本身每次都扫盘。本实现先只缓存派生结果（条目与正文映射），不缓存扫盘本身 —— 这样 `TestSkillProviderCacheInvalidatesWhenSkillChanges` 天然通过，且不会引入「缓存说没变、实际变了」这类难查的失效 bug。若后续实测扫盘成为热点，再在 `skill.System` 层加 mtime 校验，那是独立改动。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/capability/ -count=1`
Expected: `ok`

- [ ] **Step 5: 变异测试**

把上限检查 `if len(entries) > MaxSkillsPerAgent` 改成 `if false`，跑 `-run TestSkillProviderRejectsTooManySkills`。Expected: FAIL。改回。

把 `if s.Summary == ""` 那段改成 `if false`，跑 `-run TestSkillProviderRejectsSkillWithoutSummary`。Expected: FAIL。改回。

- [ ] **Step 6: 提交**

```bash
git add internal/capability/
git commit -m "feat(capability): 技能以目录条目暴露,上限 64 且必须自带一行说明

旧路径在 Summary 为空时退回注入整篇正文(cognitive/core.go:255),目录里
没有这条退路:没有一行说明的技能无法被判断该不该加载,只能让作者补上。

上限 64 是声明出来的契约而非建议。目录每轮都随缓存前缀重发,体积是常驻
成本;超限静默截断会让尾部技能列在任何地方都看不见 —— 与 claw-code 的
技能不可发现是同一种失败。"
```

---

### Task 4: 目录渲染（逐字节稳定）

**Files:**
- Create: `internal/capability/render.go`
- Test: `internal/capability/render_test.go`

**Interfaces:**
- Consumes: Task 1 的 `Entry`
- Produces: `func Render(entries []Entry) string`

- [ ] **Step 1: 写失败测试**

创建 `internal/capability/render_test.go`：

```go
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/capability/ -run TestRender -count=1`
Expected: FAIL，`undefined: capability.Render`

- [ ] **Step 3: 写最小实现**

创建 `internal/capability/render.go`：

```go
package capability

import "strings"

// Render turns a sorted entry list into the catalog block that goes into the
// prompt's stable prefix.
//
// Entries must already be sorted (Catalog.Entries does that). Render adds no
// counts, timestamps or ids of its own: anything that varies per round would
// change the cached prefix and cost a cache miss on every inference.
//
// An empty catalog renders to the empty string rather than an empty block --
// an empty listing would tell the model it has capabilities when it has none.
func Render(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n<available_capabilities>\n")
	group := ""
	for _, entry := range entries {
		if entry.Group != group {
			group = entry.Group
			b.WriteString(group)
			b.WriteString(":\n")
		}
		b.WriteString("  - ")
		b.WriteString(entry.Name)
		b.WriteString(": ")
		b.WriteString(entry.Summary)
		b.WriteString("\n")
	}
	b.WriteString("</available_capabilities>\n")
	b.WriteString("Call load_capabilities with the names you need before using them.\n")
	return b.String()
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/capability/ -count=1`
Expected: `ok`

- [ ] **Step 5: 变异测试**

把 `Render` 里写入 `entry.Summary` 的那一行删掉，跑 `go test ./internal/capability/ -run TestRenderGroupsEntriesUnderHeadings -count=1`。
Expected: FAIL（条目缺少说明文本）。确认后改回。

**关于 `TestRenderIsByteStable` 的能力边界**：`Render` 是纯函数，同一输入必然产出同一输出，因此该测试只能保证渲染无随机性（如 map 迭代序泄漏），**测不出**「目录内容随轮次变化」这类问题。真正的稳定性由 Task 7 的 `TestBuildPromptIsIdenticalAcrossTasks`（两个不同任务的目录必须逐字节相同）覆盖。不要试图在这里加一个「变异后仍应通过」的步骤 —— 那不是变异测试。

- [ ] **Step 6: 提交**

```bash
git add internal/capability/
git commit -m "feat(capability): 目录渲染,按分组排版且不含任何每轮变化的内容

渲染结果进 prompt 的缓存前缀(runtime.go:309),因此块内不放计数、时间戳
或任务 id —— 任何随轮次变化的东西都会让整段缓存作废。

空目录渲染成空串而不是空块:一个空清单会让模型以为自己有能力可用。
块尾附加加载指引,只列不说怎么用正是 claw-code 那种「技能列在那儿但没人
知道怎么拉」的失败。"
```

---

### Task 5: run state 的已加载区块与三段式 prompt

**Files:**
- Modify: `internal/runtime/runtime.go:143`（`loopState` 加 `loaded`）、`:354` 与 `:374`（prompt 组装）
- Create: `internal/runtime/loaded.go`
- Test: `internal/runtime/loaded_test.go`

**Interfaces:**
- Consumes: 无（纯 runtime 内部）
- Produces:
  - `type loadedEntry struct { name, detail string }`
  - `func renderLoaded(entries []loadedEntry) string`
  - `func appendLoaded(entries []loadedEntry, name, detail string, maxChars int) ([]loadedEntry, []string, error)` — 返回新列表、被驱逐的名字、错误
  - `func composePrompt(basePrompt string, loaded []loadedEntry, toolCtx []toolEntry, maxPromptChars int) string`

- [ ] **Step 1: 写失败测试**

创建 `internal/runtime/loaded_test.go`：

```go
package runtime

import (
	"strings"
	"testing"
)

func TestAppendLoadedIsIdempotent(t *testing.T) {
	t.Parallel()
	entries, evicted, err := appendLoaded(nil, "read_file", "SCHEMA-A", 1000)
	if err != nil {
		t.Fatalf("appendLoaded() error = %v, want nil", err)
	}
	entries, evicted, err = appendLoaded(entries, "read_file", "SCHEMA-A", 1000)
	if err != nil {
		t.Fatalf("second appendLoaded() error = %v, want nil", err)
	}
	if len(entries) != 1 {
		t.Errorf("len = %d, want 1: loading the same capability twice must replace, not accumulate", len(entries))
	}
	if len(evicted) != 0 {
		t.Errorf("evicted = %v, want none", evicted)
	}
}

func TestAppendLoadedEvictsOldestAndReportsIt(t *testing.T) {
	t.Parallel()
	entries, _, err := appendLoaded(nil, "first", strings.Repeat("a", 400), 1000)
	if err != nil {
		t.Fatalf("appendLoaded(first) error = %v", err)
	}
	entries, _, err = appendLoaded(entries, "second", strings.Repeat("b", 400), 1000)
	if err != nil {
		t.Fatalf("appendLoaded(second) error = %v", err)
	}

	entries, evicted, err := appendLoaded(entries, "third", strings.Repeat("c", 400), 1000)
	if err != nil {
		t.Fatalf("appendLoaded(third) error = %v", err)
	}
	if len(evicted) == 0 {
		t.Fatal("evicted = none, want the oldest entry to be reported")
	}
	if evicted[0] != "first" {
		t.Errorf("evicted[0] = %q, want %q (least recently loaded)", evicted[0], "first")
	}
	for _, e := range entries {
		if e.name == "first" {
			t.Error("evicted entry is still present")
		}
	}
}

func TestAppendLoadedRejectsOversizedDetail(t *testing.T) {
	t.Parallel()
	// 单个正文就超过整个区块上限时,驱逐再多也放不下。截断的 schema 是非法
	// JSON、截断的技能正文是残缺指令,两者都比明确失败更糟。
	_, _, err := appendLoaded(nil, "huge", strings.Repeat("x", 2000), 1000)
	if err == nil {
		t.Fatal("appendLoaded() error = nil, want an error naming the oversized capability")
	}
	if !strings.Contains(err.Error(), "huge") {
		t.Errorf("error = %q, want it to name the capability", err)
	}
}

func TestRenderLoadedStatesEvictions(t *testing.T) {
	t.Parallel()
	got := renderLoaded([]loadedEntry{{name: "read_file", detail: "SCHEMA"}})
	if !strings.Contains(got, "read_file") || !strings.Contains(got, "SCHEMA") {
		t.Errorf("renderLoaded() = %q, want it to carry the loaded detail", got)
	}
}

func TestComposePromptTrimsToolOutputNotLoadedBlock(t *testing.T) {
	t.Parallel()
	base := "BASE-PROMPT"
	loaded := []loadedEntry{{name: "read_file", detail: "LOADED-SCHEMA-MARKER"}}
	toolCtx := []toolEntry{{key: "k", text: strings.Repeat("t", 5000)}}

	got := composePrompt(base, loaded, toolCtx, 1000)

	if !strings.Contains(got, "BASE-PROMPT") {
		t.Error("base prompt was trimmed: the task framing must survive")
	}
	if !strings.Contains(got, "LOADED-SCHEMA-MARKER") {
		t.Error("loaded block was trimmed: a schema that silently vanishes leaves the model calling from memory")
	}
	if len([]rune(got)) > 1000+len([]rune(base))+len([]rune(renderLoaded(loaded))) {
		t.Errorf("composed prompt is %d runes, larger than the budget allows", len([]rune(got)))
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run 'TestAppendLoaded|TestRenderLoaded|TestComposePrompt' -count=1`
Expected: FAIL，`undefined: appendLoaded` 等

- [ ] **Step 3: 写最小实现**

创建 `internal/runtime/loaded.go`：

```go
package runtime

import (
	"fmt"
	"strings"
)

// loadedEntry is one capability whose full definition the model asked for.
type loadedEntry struct {
	name   string
	detail string
}

// appendLoaded adds one capability's detail to the loaded block, evicting the
// least recently loaded entries when the block would exceed maxChars.
//
// It returns the new block, the names it evicted, and an error only when the
// detail cannot fit on its own. Truncating instead would hand the model an
// invalid JSON schema or a half a skill -- both worse than a refusal it can
// see and react to.
func appendLoaded(entries []loadedEntry, name, detail string, maxChars int) ([]loadedEntry, []string, error) {
	if size := len([]rune(detail)); maxChars > 0 && size > maxChars {
		return entries, nil, fmt.Errorf("capability %q is too large to load: %d chars, limit %d", name, size, maxChars)
	}
	kept := make([]loadedEntry, 0, len(entries)+1)
	for _, e := range entries {
		if e.name != name {
			kept = append(kept, e)
		}
	}
	kept = append(kept, loadedEntry{name: name, detail: detail})

	evicted := make([]string, 0)
	for maxChars > 0 && loadedSize(kept) > maxChars && len(kept) > 1 {
		evicted = append(evicted, kept[0].name)
		kept = kept[1:]
	}
	return kept, evicted, nil
}

func loadedSize(entries []loadedEntry) int {
	total := 0
	for _, e := range entries {
		total += len([]rune(e.detail)) + len([]rune(e.name))
	}
	return total
}

// renderLoaded renders the loaded block. It is pinned: composePrompt never
// trims it, so a definition the model was given stays visible until it is
// explicitly evicted.
func renderLoaded(entries []loadedEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nLoaded capabilities:\n")
	for _, e := range entries {
		b.WriteString("- ")
		b.WriteString(e.name)
		b.WriteString(":\n")
		b.WriteString(e.detail)
		b.WriteString("\n")
	}
	return b.String()
}

// renderEvictionNotice tells the model what was dropped to make room. Silence
// here would leave it calling a capability whose definition disappeared.
func renderEvictionNotice(evicted []string) string {
	if len(evicted) == 0 {
		return ""
	}
	return fmt.Sprintf("[unloaded to free space: %s — call load_capabilities again if you still need them]\n", strings.Join(evicted, ", "))
}

// composePrompt assembles the round's prompt in three parts with separate
// budgets: the task framing and the loaded block are never trimmed, only the
// accumulated tool output is.
//
// The previous single-budget version handed base+tools to boundPrompt, which
// drops the middle -- so the task framing's tail and every early tool result
// were the first things to go.
func composePrompt(basePrompt string, loaded []loadedEntry, toolCtx []toolEntry, maxPromptChars int) string {
	loadedBlock := renderLoaded(loaded)
	if maxPromptChars <= 0 {
		return basePrompt + loadedBlock + renderToolEntries(toolCtx)
	}
	budget := maxPromptChars - len([]rune(basePrompt)) - len([]rune(loadedBlock))
	if floor := maxPromptChars / 4; budget < floor {
		budget = floor
	}
	return basePrompt + loadedBlock + boundPrompt(renderToolEntries(toolCtx), budget)
}
```

- [ ] **Step 4: 接进 loopState 与两处组装点**

在 `internal/runtime/runtime.go` 的 `loopState`（约 `:143`）加字段：

```go
	loaded           []loadedEntry
```

把 `:354` 与 `:374` 两处的

```go
		prompt := boundPrompt(st.basePrompt+renderToolEntries(st.toolCtx), r.maxPromptChars)
```

改为

```go
		prompt := composePrompt(st.basePrompt, st.loaded, st.toolCtx, r.maxPromptChars)
```

已加载区块的上限用 `r.maxPromptChars / 3`，在 Task 6 调用 `appendLoaded` 时传入。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/runtime/ -count=1`
Expected: `ok`

- [ ] **Step 6: 变异测试**

把 `composePrompt` 改回 `boundPrompt(basePrompt+loadedBlock+renderToolEntries(toolCtx), maxPromptChars)`，跑 `-run TestComposePromptTrimsToolOutputNotLoadedBlock`。
Expected: FAIL（`LOADED-SCHEMA-MARKER` 落在中段被丢）。改回。

- [ ] **Step 7: 提交**

```bash
git add internal/runtime/
git commit -m "feat(runtime): prompt 三段式,已加载区块与工具输出分开预算

原来 basePrompt 与工具输出共用一个预算交给 boundPrompt,超限时保头 1/3、
保尾 2/3、丢中段 —— 任务跑长之后,任务框架的尾巴和早先加载的内容恰好落
在中段被静默丢弃,模型继续按记忆调用,直到参数出错才暴露。

现在任务框架与已加载区块都不参与裁剪,只裁工具输出。区块自身超限时按
最久未用驱逐,并把驱逐清单写进 prompt —— 看得见才补得回来。

单个正文就超过区块上限时直接报错:截断的 schema 是非法 JSON、截断的技能
正文是残缺指令,都比一个模型能看见的失败更糟。"
```

---

### Task 6: `load_capabilities` 元工具与 `usage.Touch` 迁移

**Files:**
- Modify: `internal/runtime/lazytools.go`（元工具集与分发）
- Modify: `internal/runtime/runtime.go:580-598`（`inferenceTools`）
- Test: `internal/runtime/lazytools_load_test.go`

**Interfaces:**
- Consumes: Task 1-5 全部
- Produces:
  - `const metaToolLoadCapabilities = "load_capabilities"`
  - `const maxLoadBatch = 5`
  - `func (r *Runtime) dispatchLoadCapabilities(ctx, st *loopState, call domain.ToolCall, catalog *capability.Catalog) (domain.ToolResult, error)`
  - `Runtime` 新增字段 `skillUsage interface{ Touch(id string, at time.Time) }`（可为 nil）

- [ ] **Step 1: 写失败测试**

创建 `internal/runtime/lazytools_load_test.go`。测试直接驱动分发函数，避免拉起完整推理循环：

```go
package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/domain"
)

type stubProvider struct{ details map[string]string }

func (p stubProvider) Entries(context.Context) ([]capability.Entry, error) {
	entries := make([]capability.Entry, 0, len(p.details))
	for name := range p.details {
		entries = append(entries, capability.Entry{
			Name: name, Group: "files", Summary: "一行说明", Kind: capability.KindTool,
		})
	}
	return entries, nil
}

func (p stubProvider) Detail(_ context.Context, name string) (string, error) {
	detail, ok := p.details[name]
	if !ok {
		return "", capability.ErrUnknownCapability
	}
	return detail, nil
}

func loadCall(names ...string) domain.ToolCall {
	return domain.ToolCall{
		ID:        "c1",
		Name:      metaToolLoadCapabilities,
		Arguments: map[string]string{"names": strings.Join(names, ",")},
	}
}

func TestLoadCapabilitiesPutsDetailInLoadedBlock(t *testing.T) {
	t.Parallel()
	rt := NewRuntime(Config{})
	catalog := capability.NewCatalog(stubProvider{details: map[string]string{"read_file": "SCHEMA-MARKER"}})
	st := &loopState{}

	result, err := rt.dispatchLoadCapabilities(context.Background(), st, loadCall("read_file"), catalog)
	if err != nil {
		t.Fatalf("dispatchLoadCapabilities() error = %v, want nil", err)
	}
	if !result.Success {
		t.Fatalf("result.Success = false, error = %q", result.Error)
	}
	if len(st.loaded) != 1 || st.loaded[0].detail != "SCHEMA-MARKER" {
		t.Fatalf("loaded = %+v, want the detail pinned in the loaded block", st.loaded)
	}
	if strings.Contains(result.Output, "SCHEMA-MARKER") {
		t.Error("the detail went into the tool result: it would then be subject to the 4000-char truncation and to mid-prompt dropping")
	}
}

func TestLoadCapabilitiesRejectsUnknownName(t *testing.T) {
	t.Parallel()
	rt := NewRuntime(Config{})
	catalog := capability.NewCatalog(stubProvider{details: map[string]string{"read_file": "S"}})
	st := &loopState{}

	// 作用域检查落在这里:目录由调用方的 registry 建,Plan 模式过滤掉的
	// 工具不在目录里,因此也 load 不到。
	result, err := rt.dispatchLoadCapabilities(context.Background(), st, loadCall("write_file"), catalog)
	if err != nil {
		t.Fatalf("dispatchLoadCapabilities() error = %v, want nil (failures go back to the model)", err)
	}
	if result.Success {
		t.Fatal("result.Success = true, want a failed result for an out-of-catalog name")
	}
	if !strings.Contains(result.Error, "write_file") {
		t.Errorf("error = %q, want it to name the rejected capability", result.Error)
	}
	if len(st.loaded) != 0 {
		t.Error("a rejected load still modified the loaded block")
	}
}

func TestLoadCapabilitiesRejectsEmptyAndOversizedBatch(t *testing.T) {
	t.Parallel()
	rt := NewRuntime(Config{})
	details := map[string]string{}
	names := make([]string, 0, maxLoadBatch+1)
	for i := 0; i <= maxLoadBatch; i++ {
		name := string(rune('a' + i))
		details[name] = "S"
		names = append(names, name)
	}
	catalog := capability.NewCatalog(stubProvider{details: details})

	empty, err := rt.dispatchLoadCapabilities(context.Background(), &loopState{}, loadCall(), catalog)
	if err != nil {
		t.Fatalf("empty batch error = %v, want nil", err)
	}
	if empty.Success {
		t.Error("empty names list was accepted")
	}

	over, err := rt.dispatchLoadCapabilities(context.Background(), &loopState{}, loadCall(names...), catalog)
	if err != nil {
		t.Fatalf("oversized batch error = %v, want nil", err)
	}
	if over.Success {
		t.Errorf("batch of %d was accepted, limit is %d", len(names), maxLoadBatch)
	}
}

type recordingUsage struct{ touched []string }

func (u *recordingUsage) Touch(id string, _ time.Time) { u.touched = append(u.touched, id) }

func TestLoadCapabilitiesTouchesSkillUsage(t *testing.T) {
	t.Parallel()
	usage := &recordingUsage{}
	rt := NewRuntime(Config{SkillUsage: usage})
	catalog := capability.NewCatalog(stubProvider{details: map[string]string{"go-testing": "BODY"}})

	if _, err := rt.dispatchLoadCapabilities(context.Background(), &loopState{}, loadCall("go-testing"), catalog); err != nil {
		t.Fatalf("dispatchLoadCapabilities() error = %v", err)
	}
	// Curator 靠使用记录做老化清理,而「无使用记录的技能不会被动」
	// (skill/curator.go:153)。不 Touch 就等于 Curator 停摆且无人察觉。
	if len(usage.touched) != 1 || usage.touched[0] != "go-testing" {
		t.Errorf("touched = %v, want [go-testing]", usage.touched)
	}
}
```

（文件顶部 import 需含 `"time"`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run TestLoadCapabilities -count=1`
Expected: FAIL，`undefined: metaToolLoadCapabilities` 等

- [ ] **Step 3: 写最小实现**

在 `internal/runtime/lazytools.go`：

1. 常量增补：

```go
const (
	metaToolListTools         = "list_tools"
	metaToolCallTool          = "call_tool"
	metaToolLoadCapabilities  = "load_capabilities"
)

// maxLoadBatch bounds one load_capabilities call. A single skill body runs to
// kilobytes; five at once is already a large slice of the loaded block, and
// the model can simply call again.
const maxLoadBatch = 5
```

2. 元工具描述（加进 `metaInferenceTools`）：

```go
		{
			Name:        metaToolLoadCapabilities,
			Description: "Load the full definition of one or more capabilities listed in <available_capabilities>: a tool's parameter schema, or a skill's full instructions. Load before using. Pass a comma-separated list of names.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"names"},
				"properties": map[string]any{
					"names": map[string]any{
						"type":        "string",
						"description": "Comma-separated capability names, at most 5 per call.",
					},
				},
			},
		},
```

3. 分发实现：

```go
// dispatchLoadCapabilities pins the requested capabilities' full definitions
// into the run's loaded block.
//
// The tool result itself is only an acknowledgement. Putting the definitions
// in the result instead would subject them to the 4000-char per-result
// truncation and to the mid-prompt dropping that boundPrompt does -- a schema
// cut in half is invalid JSON, and one silently dropped leaves the model
// calling from memory.
//
// Every failure comes back as an unsuccessful ToolResult rather than a Go
// error: the model can read it and correct itself, whereas an error aborts the
// task.
func (r *Runtime) dispatchLoadCapabilities(ctx context.Context, st *loopState, call domain.ToolCall, catalog *capability.Catalog) (domain.ToolResult, error) {
	names := splitNames(call.Arguments["names"])
	if len(names) == 0 {
		return domain.ToolResult{CallID: call.ID, Success: false, Error: "load_capabilities requires at least one name"}, nil
	}
	if len(names) > maxLoadBatch {
		return domain.ToolResult{CallID: call.ID, Success: false,
			Error: fmt.Sprintf("load at most %d capabilities per call, got %d", maxLoadBatch, len(names))}, nil
	}
	maxLoadedChars := r.maxPromptChars / 3
	loadedNames := make([]string, 0, len(names))
	evictedAll := make([]string, 0)
	for _, name := range names {
		detail, err := catalog.Detail(ctx, name)
		if errors.Is(err, capability.ErrUnknownCapability) {
			return domain.ToolResult{CallID: call.ID, Success: false,
				Error: fmt.Sprintf("unknown capability %q: it is not in <available_capabilities> for this task", name)}, nil
		}
		if err != nil {
			return domain.ToolResult{}, err
		}
		next, evicted, err := appendLoaded(st.loaded, name, detail, maxLoadedChars)
		if err != nil {
			return domain.ToolResult{CallID: call.ID, Success: false, Error: err.Error()}, nil
		}
		st.loaded = next
		evictedAll = append(evictedAll, evicted...)
		loadedNames = append(loadedNames, name)
		if r.skillUsage != nil {
			r.skillUsage.Touch(name, time.Now())
		}
	}
	output := "loaded: " + strings.Join(loadedNames, ", ")
	if notice := renderEvictionNotice(evictedAll); notice != "" {
		output += "\n" + notice
	}
	return domain.ToolResult{CallID: call.ID, Success: true, Output: output}, nil
}

// splitNames parses the comma-separated names argument, dropping empties.
func splitNames(raw string) []string {
	parts := strings.Split(raw, ",")
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	return names
}
```

4. `Runtime` 增加 usage 字段。在 `internal/runtime/runtime.go` 的 `Config` 与 `Runtime` 结构体分别加：

```go
	// SkillUsage records that a skill was actually loaded. The Curator ages
	// idle skills off this record, and leaves skills with no usage history
	// alone -- so a runtime that never touches it silently disables the sweep.
	SkillUsage SkillUsageRecorder
```

```go
// SkillUsageRecorder is the usage sidecar skill.UsageStore satisfies.
type SkillUsageRecorder interface {
	Touch(id string, at time.Time)
}
```

5. 在 `dispatchToolCall` 的 meta 分支里增加一路 `case metaToolLoadCapabilities:`，并把 `list_tools` 分支删除（目录已进 prompt，不再需要它）。`call_tool` 分支保持不动。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -count=1`
Expected: `ok`

`list_tools` 的既有测试会因该工具被移除而失败。**一并删除它们**：它们断言的是已经不存在的行为，留着就是死代码。删除时在提交信息里**逐个点名删了哪些测试函数、以及它们原本测的行为现在由谁承担**（目录渲染由 `TestRenderGroupsEntriesUnderHeadings` 覆盖，明细获取由 `TestLoadCapabilitiesPutsDetailInLoadedBlock` 覆盖）—— 删测试必须留下可追溯的理由，否则与「悄悄降低覆盖」无法区分。

- [ ] **Step 5: 变异测试**

把 `dispatchLoadCapabilities` 里 `st.loaded = next` 改成把 detail 塞进 `Output`（模拟"明细走普通结果通道"），跑 `-run TestLoadCapabilitiesPutsDetailInLoadedBlock`。Expected: FAIL。改回。

把 `r.skillUsage.Touch` 那两行删掉，跑 `-run TestLoadCapabilitiesTouchesSkillUsage`。Expected: FAIL。改回。

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/
git commit -m "feat(runtime): load_capabilities 按需拉取能力明细,并接回技能使用记录

明细进已加载区块,tool result 只回一句确认。放进结果里会同时撞上两道
损伤:单条结果 4000 字符截断,以及 boundPrompt 的中段丢弃 —— 被切一半的
schema 是非法 JSON,被丢掉的则让模型凭记忆调用。

所有失败都以失败的 ToolResult 回给模型而不是 Go error:模型能读到并自行
纠正,而 error 会中止整个任务。

usage.Touch 从 SelectForTask 迁到这里。Curator 靠使用记录做老化清理,且
「无使用记录的技能不会被动」(skill/curator.go:153),不 Touch 等于 Curator
停摆且无人察觉。新口径也更准:模型真的把正文拉进上下文才算用过,而不是
关键词碰巧匹配上就算。

删除 list_tools:目录已进 system prompt,发现不再需要一次往返。"
```

---

### Task 7: 目录接进 `basePrompt` 并撤掉旧的技能注入

**Files:**
- Modify: `internal/cognitive/core.go:234-259`（`skillBlock`）
- Modify: `internal/runtime/agent_resolver.go:104` 与 `internal/cli/command.go:2189`（装配 `SkillProvider`）
- Test: `internal/cognitive/core_catalog_test.go`

**Interfaces:**
- Consumes: Task 1-4 的 `Catalog` / `Render` / `NewSkillProvider` / `NewToolProvider`
- Produces: `Core` 的 `WithSkills` 改为接受 `*capability.Catalog`（或新增 `WithCatalog`），`skillBlock` 输出 `capability.Render` 的结果

- [ ] **Step 1: 写失败测试**

创建 `internal/cognitive/core_catalog_test.go`：

```go
package cognitive_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/domain"
)

type catalogStub struct{ entries []capability.Entry }

func (s catalogStub) Entries(context.Context) ([]capability.Entry, error) { return s.entries, nil }
func (s catalogStub) Detail(context.Context, string) (string, error) {
	return "", capability.ErrUnknownCapability
}

func TestBuildPromptCarriesFullCatalogNotSelectedSkills(t *testing.T) {
	t.Parallel()
	catalog := capability.NewCatalog(catalogStub{entries: []capability.Entry{
		{Name: "go-testing", Group: "skills", Summary: "写 Go 测试", Kind: capability.KindSkill},
		{Name: "unrelated-skill", Group: "skills", Summary: "与任务无关", Kind: capability.KindSkill},
	}})
	core := cognitive.New().WithCatalog(catalog)

	prompt, err := core.BuildPrompt(context.Background(), cognitive.Request{
		Task: domain.Task{ID: "t1", Input: "写点 Go 测试"},
	})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v, want nil", err)
	}

	// 全量目录:与任务关键词无关的技能也必须在,否则又退回「系统替模型猜」。
	for _, want := range []string{"go-testing", "unrelated-skill"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q; the catalog must list every skill, not a keyword-matched subset", want)
		}
	}
}

func TestBuildPromptIsIdenticalAcrossTasks(t *testing.T) {
	t.Parallel()
	catalog := capability.NewCatalog(catalogStub{entries: []capability.Entry{
		{Name: "go-testing", Group: "skills", Summary: "写 Go 测试", Kind: capability.KindSkill},
	}})
	core := cognitive.New().WithCatalog(catalog)
	ctx := context.Background()

	first, err := core.BuildPrompt(ctx, cognitive.Request{Task: domain.Task{ID: "t1", Input: "写测试"}})
	if err != nil {
		t.Fatalf("first BuildPrompt() error = %v", err)
	}
	second, err := core.BuildPrompt(ctx, cognitive.Request{Task: domain.Task{ID: "t2", Input: "完全不同的输入"}})
	if err != nil {
		t.Fatalf("second BuildPrompt() error = %v", err)
	}

	firstCatalog := extractBlock(first)
	secondCatalog := extractBlock(second)
	// 目录进的是 prompt 缓存前缀。两个任务之间目录若不同,跨任务缓存必然 miss。
	if firstCatalog != secondCatalog {
		t.Errorf("catalog differs across tasks:\n%q\nvs\n%q", firstCatalog, secondCatalog)
	}
}

func extractBlock(prompt string) string {
	start := strings.Index(prompt, "<available_capabilities>")
	end := strings.Index(prompt, "</available_capabilities>")
	if start < 0 || end < 0 {
		return ""
	}
	return prompt[start : end+len("</available_capabilities>")]
}
```

**注意**：`cognitive.New()` / `BuildPrompt` / `Request` 的真实签名以 `internal/cognitive/core.go` 为准。实施时先读该文件确认，若名字不同则改测试里的调用，**不要为迁就测试改动生产签名**。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cognitive/ -run TestBuildPrompt -count=1`
Expected: FAIL，`core.WithCatalog undefined`

- [ ] **Step 3: 写最小实现**

改 `internal/cognitive/core.go`：

1. 新增字段与构造方法：

```go
// WithCatalog attaches the capability catalog. Its rendering goes into the
// task framing, which is the prompt's stable prefix -- so it must be the same
// for every task of the same agent, and it is: the catalog lists everything,
// not a per-task selection.
func (c *Core) WithCatalog(catalog *capability.Catalog) *Core {
	c.catalog = catalog
	return c
}
```

2. `skillBlock` 整体替换为：

```go
// catalogBlock renders the capability catalog into the prompt.
//
// It replaces the previous keyword-matched injection, which scored skills
// against task.Input, took the top three, and fell back to injecting a skill's
// entire body when it declared no summary. That made the framing different for
// every task -- so the provider prompt cache missed on every task -- and let
// the system guess on the model's behalf. The catalog lists everything in one
// line each; the model pulls what it wants with load_capabilities.
func (c *Core) catalogBlock(ctx context.Context) (string, error) {
	if c.catalog == nil {
		return "", nil
	}
	entries, err := c.catalog.Entries(ctx)
	if err != nil {
		return "", fmt.Errorf("build capability catalog: %w", err)
	}
	return capability.Render(entries), nil
}
```

3. 原先调用 `skillBlock` 的地方改调 `catalogBlock`；`WithSkills` 与 `SkillProvider` 接口保留但不再参与 prompt 构建，并在其 doc 注释写明：

```go
// WithSkills attaches the skill selector.
//
// Deprecated for prompt building: skills now reach the model through the
// capability catalog (WithCatalog). SelectForTask is kept for the /skills
// query paths and for future catalog search scoring; it no longer injects
// anything into a prompt.
```

4. 装配点：`internal/runtime/agent_resolver.go:104` 与 `internal/cli/command.go:2189` 把 `WithSkills(skill.NewSystem(...))` 改为同时构造 catalog：

```go
	skillSystem := skill.NewSystem(skill.Config{ /* 原有参数不变 */ })
	contextBuilder = contextBuilder.WithCatalog(capability.NewCatalog(
		capability.NewToolProvider(tools),
		capability.NewSkillProvider(skillSystem),
	))
```

**注意**：`tools` 必须是该任务实际使用的 registry（`agent_resolver.go` 里是刚构造的 per-agent registry），不能用全局的 —— 否则目录会广告出该任务无权运行的工具。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/cognitive/ ./internal/runtime/ ./internal/cli/ -count=1`
Expected: 三个包 `ok`。既有断言「top-3 技能注入」的测试会失败 —— 它们测的是被替换掉的行为，改为断言目录存在。

- [ ] **Step 5: 变异测试**

把 `catalogBlock` 改回调用 `SelectForTask(ctx, req.Task, 3)` 并注入，跑 `-run TestBuildPromptIsIdenticalAcrossTasks`。
Expected: FAIL（两个任务的块不同）。改回。

- [ ] **Step 6: 提交**

```bash
git add internal/cognitive/ internal/runtime/agent_resolver.go internal/cli/command.go
git commit -m "feat(cognitive): 技能改由能力目录进 prompt,不再按关键词猜 top 3

旧实现按 task.Input 给技能打分取前三注入,且在 Summary 为空时注入整篇
正文。两个后果:系统替模型猜该用哪个技能;每个任务的任务框架都不同,
而框架正是 prompt 缓存的稳定前缀,于是跨任务缓存必然 miss。

改为全量目录、每个技能一行,模型自己用 load_capabilities 拉正文。目录对
同一 agent 跨任务完全一致,前缀更长且更稳,缓存命中率反而上升。

目录用调用方的 registry 构建,不用全局的 —— 否则会广告出该任务无权运行
的工具。SelectForTask 保留供 /skills 查询与将来的目录检索打分复用,doc
注释已写明它不再参与 prompt 注入。"
```

---

### Task 8: checkpoint 迁移、子 runtime 空集与作用域安全测试

**Files:**
- Modify: `internal/runtime/runtime.go:413`（快照）、`:286`（恢复）
- Modify: `internal/runtime/delegation.go:96` 附近（`newSubRuntime`）
- Modify: `internal/sessionstate/`（`Checkpoint` 结构与 schema 版本）
- Test: `internal/runtime/capability_scope_test.go`

**Interfaces:**
- Consumes: Task 5-7 全部
- Produces: `Checkpoint` 新增 `Loaded []LoadedCapability`，schema 版本 +1

- [ ] **Step 1: 写失败测试**

创建 `internal/runtime/capability_scope_test.go`：

```go
package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/tool"
)

// TestPlanModeCatalogExcludesSensitiveTools pins the boundary that keeps the
// meta tools from becoming a way around Plan mode. effectiveTools already
// drops the side-effecting tools; if the catalog were built from the full
// registry instead, the model could load a sensitive tool's schema and call it.
func TestPlanModeCatalogExcludesSensitiveTools(t *testing.T) {
	t.Parallel()
	registry := tool.NewRegistry(nil, nil, tool.Guardrails{})
	noop := tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, nil
	})
	registry.RegisterDescriptor(tool.Descriptor{
		Name: "read_file", Group: "files", Description: "Read.", InputSchema: map[string]any{"type": "object"},
	}, noop)
	registry.RegisterDescriptor(tool.Descriptor{
		Name: "write_file", Group: "files", Description: "Write.", Sensitive: true, InputSchema: map[string]any{"type": "object"},
	}, noop)

	rt := NewRuntime(Config{Tools: registry})
	effective := rt.effectiveTools(domain.Task{ID: "t1", Mode: domain.ModePlan})
	catalog := capability.NewCatalog(capability.NewToolProvider(effective))

	entries, err := catalog.Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries() error = %v, want nil", err)
	}
	for _, e := range entries {
		if e.Name == "write_file" {
			t.Fatal("Plan-mode catalog lists a sensitive tool: load+call would bypass the read-only restriction")
		}
	}

	st := &loopState{}
	result, err := rt.dispatchLoadCapabilities(context.Background(), st, loadCall("write_file"), catalog)
	if err != nil {
		t.Fatalf("dispatchLoadCapabilities() error = %v", err)
	}
	if result.Success {
		t.Fatal("loading a tool outside the task's effective registry succeeded")
	}
	if !strings.Contains(result.Error, "write_file") {
		t.Errorf("error = %q, want it to name the refused tool", result.Error)
	}
}

func TestSubRuntimeStartsWithEmptyLoadedSet(t *testing.T) {
	t.Parallel()
	parent := NewRuntime(Config{})
	child := parent.newSubRuntime()
	if child == nil {
		t.Fatal("newSubRuntime() = nil")
	}
	// 子 agent 是独立上下文:继承父任务已加载的能力会把无关定义带进去,
	// 也让父子的上下文预算互相污染。
	if child.parentLoadedLeaked() {
		t.Error("sub runtime inherited the parent's loaded capabilities")
	}
}
```

**注意**：`newSubRuntime` 的真实签名与 `parentLoadedLeaked` 需按实际实现调整 —— 若 `loaded` 存在 `loopState` 而非 `Runtime` 上，子 runtime 天然不继承，此时把该测试改为断言「子 runtime 的新 loopState 的 loaded 为空」，并在 `newSubRuntime` 处补注释说明这一点是**结构决定的**而非偶然。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run 'TestPlanModeCatalog|TestSubRuntime' -count=1`
Expected: FAIL

- [ ] **Step 3: checkpoint 存取**

在 `internal/sessionstate` 的 `Checkpoint` 加：

```go
	// Loaded carries the capabilities whose full definitions the model pulled
	// during this run, so a resumed task does not have to rediscover them.
	Loaded []LoadedCapability
```

```go
// LoadedCapability is one entry of the loaded block, persisted verbatim.
type LoadedCapability struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}
```

schema 版本 +1。恢复时 `Loaded` 为空是合法的（旧 checkpoint），模型可重新加载。

在 `runtime.go:413` 的快照处加 `Loaded: snapshotLoaded(st.loaded)`，`:286` 的恢复处加 `loaded: restoreLoaded(cp.Loaded)`，并实现这两个转换函数（与既有 `snapshotToolEntries` / `restoreToolEntries` 同形）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ ./internal/sessionstate/ -count=1`
Expected: `ok`

- [ ] **Step 5: 全量门禁 + race**

```bash
gofmt -l .
go build ./... && go vet ./... && go test ./...
PATH="/c/Users/Administrator/AppData/Local/Microsoft/WinGet/Packages/MartinStorsjo.LLVM-MinGW.MSVCRT_Microsoft.Winget.Source_8wekyb3d8bbwe/llvm-mingw-20260616-msvcrt-x86_64/bin:$PATH" CGO_ENABLED=1 go test -race ./internal/capability/ ./internal/runtime/ ./internal/cognitive/ ./internal/tool/ ./internal/cli/
```

Expected: `gofmt` 无输出；build/vet/test 全绿；race 全绿。

- [ ] **Step 6: 变异测试**

把 `TestPlanModeCatalogExcludesSensitiveTools` 里的 `rt.effectiveTools(...)` 换成直接用全量 `registry` 建目录 —— 这模拟「目录读全局 registry」这个错误。
Expected: FAIL（`write_file` 出现在目录里）。改回。

- [ ] **Step 7: 提交**

```bash
git add internal/runtime/ internal/sessionstate/
git commit -m "feat(runtime): 已加载能力随 checkpoint 持久化,并钉住作用域不变量

已加载的明细进 checkpoint,挂起审批与进程重启之后不必重新拉一遍。旧
checkpoint 的 Loaded 为空是合法状态,模型重新加载即可。

新增作用域测试:Plan 模式下 sensitive 工具既不出现在目录里、也 load 不到。
目录、load、call 三者必须用调用方传入的 effective registry —— 读全局
registry 会让元工具变成绕过 Plan 只读限制的通道。这是 hermes-agent 用
test_tool_call_rejects_out_of_scope_tool 专门钉住的同一条边界。"
```

---

## 计划自查

**Spec 覆盖检查**

| Spec 章节 | 对应任务 |
|---|---|
| §4.1 `capability` 包与两个 provider | Task 1 / 2 / 3 |
| §4.2 三条边界（不执行、技能不进 registry、目录无状态） | Task 1（不执行、去重）、Task 3（不进 registry） |
| §4.3 `lazytools.go` 拆分 | Task 6（目录渲染已移出，`list_tools` 删除） |
| §5.1 已加载区块 | Task 5 / 6 |
| §5.2 三段式与预算 | Task 5 |
| §5.4 子 runtime | Task 8 |
| §6.1 缓存 | Task 3（派生结果缓存；扫盘缓存明确留待实测） |
| §6.2 目录进稳定前缀 | Task 7 |
| §7.1 格式 | Task 4 |
| §7.2 四条条目契约 | Task 1（Summary 必填/限长）、Task 2（Group 必填）、Task 3（上限 64） |
| §8.1 `load_capabilities` 四条失败语义 | Task 6 |
| §10 作用域安全 | Task 8 |
| §11.2 `usage.Touch` 迁移 | Task 6 |
| §11.3 checkpoint 迁移 | Task 8 |
| §12 验收 1（tools 数组变小） | **未覆盖** —— 属阈值门控，见下 |
| §12 验收 7（`lazy_tools` 报错） | **未覆盖** —— 属第二部分 |

**已知缺口（有意留给第二部分计划）**：§9 阈值门控、§8.2 `search_capabilities`、§8 的 `call_tool` 按条件注册、§11.1 `lazy_tools` 墓碑、§12 验收第 1 与第 7 条。这些均依赖本计划建立的 `Catalog`，且不影响本计划交付后的可用性 —— 本计划完成后，技能侧闭环即可工作。

**类型一致性检查**：`Entry` / `Provider` / `Catalog` / `ErrUnknownCapability` / `MaxSummaryChars`（Task 1）→ 被 Task 2、3、6、7、8 使用，名称一致。`loadedEntry` / `appendLoaded` / `renderLoaded` / `composePrompt`（Task 5）→ 被 Task 6、8 使用，签名一致。`metaToolLoadCapabilities` / `maxLoadBatch`（Task 6）→ 被 Task 8 的测试使用。

**实施者须先核对的三处真实签名**（计划中已就地标注，不得为迁就测试改动生产签名）：`skill.Config` 的字段名与 front matter 键名（Task 3 Step 2）、`cognitive.New`/`BuildPrompt`/`Request` 的签名（Task 7 Step 1）、`newSubRuntime` 的签名与 `loaded` 的实际归属（Task 8 Step 1）。
