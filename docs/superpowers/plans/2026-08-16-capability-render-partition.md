# 能力目录分区渲染实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让插件贡献的能力目录条目恒定排在所有内建条目之后，使插件增删只改动 prompt 缓存前缀的尾部而非中段。

**Architecture:** `capability.Entry` 增加 `Origin` 字段（零值 = 内建），`Catalog.Entries` 的排序键从 `(Group, Name)` 变为 `(Origin, Group, Name)`。`Render` 只需处理一处新情况——同名 `Group` 出现在两个 `Origin` 分区时应各自输出一次组标题。核心验收是一条不变量：**任意插件条目增删，渲染结果中内建部分保持 byte-identical。**

**Tech Stack:** Go 1.26.0，module `github.com/stardust/legion-agent`，标准库 `sort`/`strings`，测试用 `go test`。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：禁止 fallback、禁止静默吞错；不变量违反用 `panic`，业务错误返回包装过的 error。
- 公开 API 必须有 Go doc 注释，以标识符名开头。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿，`gofmt -l .` 输出为空。
- **不得引入任何随每轮变化的内容**（计数、时间戳、id）到渲染结果——`Render` 现有注释与 `Catalog.Entries` 的排序注释都写明了这条，它是本计划存在的前提。
- 背景与语义依据：`docs/agents/bug/2026-08-16-prompt-cache-backend-mismatch.md`（Legion 后端是 DeepSeek，缓存是**从第 0 token 起的最长公共前缀**，因此把变化点后移是直接有效的缓解）。

---

### Task 1: `Origin` 字段与分区排序

**Files:**
- Modify: `internal/capability/catalog.go`（`Entry` 结构体、新增 `Origin` 类型、`Entries` 排序）
- Test: `internal/capability/catalog_partition_test.go`（新建）

**Interfaces:**
- Consumes: 无
- Produces:
  - `type Origin uint8`
  - `const OriginBuiltin Origin = iota` / `OriginPlugin`
  - `func (o Origin) String() string`
  - `Entry.Origin Origin`（新字段，零值 = `OriginBuiltin`）

- [ ] **Step 1: 写失败测试**

创建 `internal/capability/catalog_partition_test.go`：

```go
package capability_test

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
)

// partitionProvider serves a fixed entry list.
type partitionProvider struct{ entries []capability.Entry }

func (p partitionProvider) Entries(context.Context) ([]capability.Entry, error) {
	return p.entries, nil
}

func (p partitionProvider) Detail(context.Context, string) (string, error) {
	return "", capability.ErrUnknownCapability
}

func builtinEntries() []capability.Entry {
	return []capability.Entry{
		{Name: "read_file", Group: "files", Summary: "Read a file", Kind: capability.KindTool},
		{Name: "write_file", Group: "files", Summary: "Write a file", Kind: capability.KindTool},
		{Name: "web_search", Group: "web", Summary: "Search the web", Kind: capability.KindTool},
	}
}

// A plugin group whose name sorts BEFORE every builtin group, to prove the
// partition key outranks the group name.
func pluginEntries() []capability.Entry {
	return []capability.Entry{
		{Name: "jira_search", Group: "aaa-jira", Summary: "Search Jira", Kind: capability.KindTool,
			Origin: capability.OriginPlugin},
	}
}

func names(entries []capability.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

// Origin outranks Group: a plugin group named "aaa-jira" still sorts after
// every builtin group, so plugin entries never land in the middle of the
// cached prefix.
func TestPluginEntriesSortAfterBuiltinRegardlessOfGroupName(t *testing.T) {
	t.Parallel()
	catalog := capability.NewCatalog(
		partitionProvider{entries: pluginEntries()},
		partitionProvider{entries: builtinEntries()},
	)
	got, err := catalog.Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	want := []string{"read_file", "write_file", "web_search", "jira_search"}
	gotNames := names(got)
	if len(gotNames) != len(want) {
		t.Fatalf("want %v, got %v", want, gotNames)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("want %v, got %v", want, gotNames)
		}
	}
}

// The zero value is builtin, so existing providers keep their behavior with no
// code change.
func TestZeroOriginIsBuiltin(t *testing.T) {
	t.Parallel()
	var e capability.Entry
	if e.Origin != capability.OriginBuiltin {
		t.Fatalf("zero Origin must be OriginBuiltin, got %v", e.Origin)
	}
}

// Within one partition the existing (group, name) ordering is unchanged.
func TestOrderingWithinPartitionUnchanged(t *testing.T) {
	t.Parallel()
	catalog := capability.NewCatalog(partitionProvider{entries: []capability.Entry{
		{Name: "write_file", Group: "files", Summary: "Write a file", Kind: capability.KindTool},
		{Name: "web_search", Group: "web", Summary: "Search the web", Kind: capability.KindTool},
		{Name: "read_file", Group: "files", Summary: "Read a file", Kind: capability.KindTool},
	}})
	got, err := catalog.Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	want := []string{"read_file", "write_file", "web_search"}
	for i, name := range names(got) {
		if name != want[i] {
			t.Fatalf("want %v, got %v", want, names(got))
		}
	}
}

func TestOriginString(t *testing.T) {
	t.Parallel()
	if capability.OriginBuiltin.String() != "builtin" {
		t.Fatalf("got %q", capability.OriginBuiltin.String())
	}
	if capability.OriginPlugin.String() != "plugin" {
		t.Fatalf("got %q", capability.OriginPlugin.String())
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/capability/ -run "Partition|ZeroOrigin|OriginString|OrderingWithin" -v
```

预期：编译失败，`unknown field Origin in struct literal` / `undefined: capability.OriginPlugin`。

- [ ] **Step 3: 实现**

在 `internal/capability/catalog.go` 的 `Kind` 定义之后加入：

```go
// Origin says whether a capability ships with the agent or comes from a
// dynamically loaded plugin. It is the catalog's primary sort key, ahead of
// group and name.
//
// The ordering is a prompt-cache property, not cosmetics: the rendered catalog
// sits at the head of the request, and DeepSeek-style caching matches the
// longest common prefix from the very first token. Sorting every plugin entry
// after every builtin one confines a plugin load or unload to the tail of that
// prefix, so everything before it still hits the cache. A plugin group whose
// name happens to sort early (say "aaa-jira") must not be able to move the
// change point into the middle of the builtin listing — hence a dedicated key
// rather than a naming convention.
type Origin uint8

const (
	// OriginBuiltin is the zero value: an entry that ships with the agent.
	OriginBuiltin Origin = iota
	// OriginPlugin is an entry contributed by a dynamically loaded plugin.
	OriginPlugin
)

// String returns the lowercase name used in diagnostics.
func (o Origin) String() string {
	switch o {
	case OriginBuiltin:
		return "builtin"
	case OriginPlugin:
		return "plugin"
	default:
		return fmt.Sprintf("origin(%d)", uint8(o))
	}
}
```

在 `Entry` 结构体加字段：

```go
type Entry struct {
	Name    string
	Group   string
	Summary string
	Kind    Kind
	// Origin partitions the catalog; the zero value is OriginBuiltin, so a
	// provider that predates plugins needs no change. See Origin.
	Origin Origin
}
```

`Entries` 的排序改为三级键：

```go
	sort.Slice(all, func(i, j int) bool {
		if all[i].Origin != all[j].Origin {
			return all[i].Origin < all[j].Origin
		}
		if all[i].Group == all[j].Group {
			return all[i].Name < all[j].Name
		}
		return all[i].Group < all[j].Group
	})
```

同时更新 `Entries` 的文档注释，把「sorted by (group, name)」改为「sorted by (origin, group, name)」，并在 `NewCatalog` 的注释里同步这句。

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/capability/ -v
```

预期：新测试全 PASS，既有 `render_test.go` / `catalog_test.go` 不受影响（零值即内建）。

- [ ] **Step 5: 提交**

```bash
git add internal/capability/catalog.go internal/capability/catalog_partition_test.go
git commit -m "feat(capability): partition catalog entries by origin before group"
```

---

### Task 2: `Render` 处理跨分区同名分组

**Files:**
- Modify: `internal/capability/render.go`
- Test: `internal/capability/render_partition_test.go`（新建）

**Interfaces:**
- Consumes: Task 1 的 `Entry.Origin`、`OriginBuiltin`/`OriginPlugin`
- Produces: 行为变更，无新导出符号

**背景：** `Render` 用「`entry.Group` 与上一条不同就输出组标题」判断分组边界。分区后，内建与插件可能各有一个同名 `Group`（例如两边都叫 `web`），它们在排序后不相邻但组名相同——若只比较组名，第二个分区的组标题会被吞掉，其条目挂到上一个标题下，读起来像是内建能力。

- [ ] **Step 1: 写失败测试**

创建 `internal/capability/render_partition_test.go`：

```go
package capability_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
)

func partitionedEntries() []capability.Entry {
	// Already in (origin, group, name) order, as Catalog.Entries returns them.
	return []capability.Entry{
		{Name: "read_file", Group: "files", Summary: "Read a file", Kind: capability.KindTool},
		{Name: "web_search", Group: "web", Summary: "Search the web", Kind: capability.KindTool},
		{Name: "jira_search", Group: "web", Summary: "Search Jira", Kind: capability.KindTool,
			Origin: capability.OriginPlugin},
	}
}

// A plugin group with the same name as a builtin group must get its own
// heading. Without it the plugin entry reads as a builtin capability.
func TestRenderRepeatsHeadingAcrossOriginBoundary(t *testing.T) {
	t.Parallel()
	got := capability.Render(partitionedEntries())
	if n := strings.Count(got, "web:\n"); n != 2 {
		t.Fatalf("want the shared group heading twice (once per origin), got %d:\n%s", n, got)
	}
	// Order still holds: the plugin entry is last.
	if idx := strings.Index(got, "jira_search"); idx < strings.Index(got, "web_search") {
		t.Fatalf("plugin entry must render after builtin entries:\n%s", got)
	}
}

// THE core invariant: adding or removing plugin entries must not change one
// byte of the builtin portion of the render. This is what keeps the cached
// prefix hitting on every round.
func TestBuiltinPortionIsByteIdenticalAcrossPluginChanges(t *testing.T) {
	t.Parallel()
	builtinOnly := []capability.Entry{
		{Name: "read_file", Group: "files", Summary: "Read a file", Kind: capability.KindTool},
		{Name: "web_search", Group: "web", Summary: "Search the web", Kind: capability.KindTool},
	}
	withPlugins := append(append([]capability.Entry{}, builtinOnly...),
		// Deliberately early-sorting group name; Task 1's key must keep it last.
		capability.Entry{Name: "jira_search", Group: "aaa-jira", Summary: "Search Jira",
			Kind: capability.KindTool, Origin: capability.OriginPlugin},
		capability.Entry{Name: "gitlab_mr", Group: "aaa-gitlab", Summary: "List MRs",
			Kind: capability.KindTool, Origin: capability.OriginPlugin},
	)

	base := capability.Render(builtinOnly)
	// The builtin prefix ends where the closing tag begins in the plugin-free
	// render; everything before that must survive verbatim.
	head := base[:strings.Index(base, "</available_capabilities>")]

	full := capability.Render(sortForTest(withPlugins))
	if !strings.HasPrefix(full, head) {
		t.Fatalf("builtin portion changed when plugins were added.\nwant prefix:\n%q\ngot:\n%q", head, full)
	}

	// And removing them restores the original bytes exactly.
	if again := capability.Render(builtinOnly); again != base {
		t.Fatalf("unloading plugins must restore the identical render")
	}
}

// sortForTest mirrors Catalog.Entries' ordering so this test exercises Render
// against the input shape it actually receives.
func sortForTest(entries []capability.Entry) []capability.Entry {
	out := append([]capability.Entry{}, entries...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Origin != out[j].Origin {
			return out[i].Origin < out[j].Origin
		}
		if out[i].Group == out[j].Group {
			return out[i].Name < out[j].Name
		}
		return out[i].Group < out[j].Group
	})
	return out
}
```

补上 `"sort"` import。

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/capability/ -run "RenderRepeatsHeading|BuiltinPortionIsByteIdentical" -v
```

预期：`TestRenderRepeatsHeadingAcrossOriginBoundary` FAIL（`want the shared group heading twice ... got 1`）。`TestBuiltinPortionIsByteIdentical...` 在 Task 1 已完成的前提下应当已经 PASS——它验证的是排序键的效果；若 FAIL 说明 Task 1 的排序有问题，先回头修。

- [ ] **Step 3: 实现**

`internal/capability/render.go` 的循环改为同时跟踪 origin：

```go
	var b strings.Builder
	b.WriteString("\n\n<available_capabilities>\n")
	group := ""
	origin := OriginBuiltin
	first := true
	for _, entry := range entries {
		// A heading is emitted whenever either key changes. Origin matters even
		// when the group name repeats: a plugin group sharing a builtin group's
		// name is a different section, and merging them would present a plugin
		// capability as a builtin one.
		if first || entry.Group != group || entry.Origin != origin {
			group = entry.Group
			origin = entry.Origin
			b.WriteString(group)
			b.WriteString(":\n")
		}
		first = false
		b.WriteString("  - ")
		b.WriteString(entry.Name)
		b.WriteString(": ")
		b.WriteString(entry.Summary)
		b.WriteString("\n")
	}
```

> 注意 `first` 标志是必需的：原实现用 `group := ""` 配合「组名非空」隐式保证首条一定输出标题，加入 origin 比较后 `OriginBuiltin` 是零值，首条若组名恰好为空字符串将不输出标题——`Entry.Validate` 已禁止空组名，但不要依赖另一处的校验来维持这里的正确性。

同时更新 `Render` 的文档注释，说明「分区边界也会重新输出组标题」。

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/capability/ -v
```

预期：全部 PASS，包括既有的 `TestRenderIsByteStable` 与 `TestRenderGroupsEntriesUnderHeadings`。

- [ ] **Step 5: 全量回归**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l .
```

预期：全绿，`gofmt -l .` 无输出。重点关注 `internal/cognitive`（`catalogBlock` 的调用方）与 `internal/runtime`。

- [ ] **Step 6: 提交**

```bash
git add internal/capability/render.go internal/capability/render_partition_test.go
git commit -m "fix(capability): emit a group heading at each origin boundary"
```

---

### Task 3: 把不变量固定在 `cognitive` 层

**Files:**
- Test: `internal/cognitive/catalog_prefix_test.go`（新建）

**Interfaces:**
- Consumes: Task 1/2 的分区排序与渲染；`cognitive.Core.BuildContext`、`BuiltContext.StablePrefixLen`
- Produces: 无生产代码，仅回归测试

**背景：** Task 1/2 保证的是 `capability` 包内部的行为。真正要守住的不变量在更上一层：**插件增删后，`BuildContext` 产出的 stable prefix 的内建部分保持 byte-identical**。这条测试是这个计划的验收依据，也是将来任何人改动 `catalogBlock` 或 prefix 组装顺序时的护栏。

- [ ] **Step 1: 写测试**

创建 `internal/cognitive/catalog_prefix_test.go`（`package cognitive`，需要访问 `BuildContext` 与 `NewThresholdCompressor`）：

```go
package cognitive

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/domain"
)

type prefixProvider struct{ entries []capability.Entry }

func (p prefixProvider) Entries(context.Context) ([]capability.Entry, error) {
	return p.entries, nil
}

func (p prefixProvider) Detail(context.Context, string) (string, error) {
	return "", capability.ErrUnknownCapability
}

func buildPrefix(t *testing.T, entries []capability.Entry) string {
	t.Helper()
	core := NewCore(NewThresholdCompressor(1 << 20)) // never compress: keep the offset
	built, err := core.BuildContext(context.Background(), Request{
		Agent:   domain.Agent{ID: "agent-1", Role: "developer"},
		Task:    domain.Task{ID: "task-1", Input: "do the thing"},
		Catalog: capability.NewCatalog(prefixProvider{entries: entries}),
	})
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	return string([]rune(built.Prompt)[:built.StablePrefixLen])
}

func prefixBuiltins() []capability.Entry {
	return []capability.Entry{
		{Name: "read_file", Group: "files", Summary: "Read a file", Kind: capability.KindTool},
		{Name: "web_search", Group: "web", Summary: "Search the web", Kind: capability.KindTool},
	}
}

// Loading a plugin must only append to the cache-stable prefix, never rewrite
// its head. Under DeepSeek-style longest-common-prefix caching, everything
// before the change point still hits.
// See docs/agents/bug/2026-08-16-prompt-cache-backend-mismatch.md
func TestPluginLoadOnlyAppendsToStablePrefix(t *testing.T) {
	before := buildPrefix(t, prefixBuiltins())

	withPlugin := append(append([]capability.Entry{}, prefixBuiltins()...),
		capability.Entry{Name: "jira_search", Group: "aaa-jira", Summary: "Search Jira",
			Kind: capability.KindTool, Origin: capability.OriginPlugin})
	after := buildPrefix(t, withPlugin)

	shared := commonPrefixLen(before, after)
	if shared == len([]rune(before)) {
		return // whole builtin prefix survived — the goal
	}
	t.Fatalf("plugin load rewrote the builtin prefix: only %d of %d runes survived\nbefore:\n%s\nafter:\n%s",
		shared, len([]rune(before)), before, after)
}

// Unloading restores the original bytes, so a load/unload cycle costs at most
// two partial misses rather than permanently shifting the prefix.
func TestPluginUnloadRestoresStablePrefix(t *testing.T) {
	before := buildPrefix(t, prefixBuiltins())
	withPlugin := append(append([]capability.Entry{}, prefixBuiltins()...),
		capability.Entry{Name: "jira_search", Group: "aaa-jira", Summary: "Search Jira",
			Kind: capability.KindTool, Origin: capability.OriginPlugin})
	_ = buildPrefix(t, withPlugin)

	if after := buildPrefix(t, prefixBuiltins()); after != before {
		t.Fatalf("unload did not restore the identical prefix")
	}
}

func commonPrefixLen(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	n := len(ra)
	if len(rb) < n {
		n = len(rb)
	}
	for i := 0; i < n; i++ {
		if ra[i] != rb[i] {
			return i
		}
	}
	return n
}
```

- [ ] **Step 2: 运行**

```bash
go test ./internal/cognitive/ -run "PluginLoadOnlyAppends|PluginUnloadRestores" -v
```

预期：两个测试 PASS（Task 1/2 已经保证了排序与渲染）。

- [ ] **Step 3: 变异验证测试确有效**

临时把 Task 1 的排序键退回两级，确认测试能捕获：

```bash
cp internal/capability/catalog.go internal/capability/catalog.go.bak
sed -i 's|if all\[i\].Origin != all\[j\].Origin {|if false {|' internal/capability/catalog.go
go test ./internal/cognitive/ -run "PluginLoadOnlyAppends" -count=1
cp internal/capability/catalog.go.bak internal/capability/catalog.go && rm internal/capability/catalog.go.bak
```

预期：变异下 FAIL（`plugin load rewrote the builtin prefix`），还原后 PASS。若变异下仍 PASS，说明测试用的插件组名排序位置没有真正越过内建组——换一个更靠前的组名重试。

- [ ] **Step 4: 全量回归 + 提交**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l .
```

```bash
git add internal/cognitive/catalog_prefix_test.go
git commit -m "test(cognitive): pin the plugin-append-only invariant on the stable prefix"
```

---

## 交付后状态

- 插件条目恒定排在内建条目之后，与其 group 名无关。
- 插件增删只改动 prompt 缓存前缀的尾部；实测口径下内建部分 byte-identical。
- 卸载后前缀完全还原。
- 该不变量由 `cognitive` 层的回归测试守住，且已用变异验证证明测试有效。

**本计划不包含**（各自独立）：

| 项 | 去处 |
|---|---|
| 实测 `cache_control` 在 DeepSeek 上是被忽略还是被拒绝 | `docs/agents/bug/2026-08-16-prompt-cache-backend-mismatch.md` § 待验证 |
| 记录 `usage.prompt_cache_hit_tokens` 以获得命中率基线 | 同上 |
| `StablePrefixLen` / `cache_control` 链路的去留决策 | 同上 |
| 插件变更只在任务边界生效 | 插件系统 P1，见 `docs/design/architecture/legion-plugin-system.md` |
| 插件贡献条目登记进 `toolauth` gateable | 同上 §6.11 |
