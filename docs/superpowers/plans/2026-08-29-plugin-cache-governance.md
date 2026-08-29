# 插件缓存治理 实施计划（G6）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 缓存目录不再是一个只进不出的黑箱——能看见里面有什么、能删掉不要的、而验签失败的包不再永久躺在磁盘上。

**Architecture:** 两层。**机制**在 `internal/plugin/fetch`（`Cache.List` / `Cache.Remove`，与 `Put` 共用同一把跨进程 digest 锁）；**策略**在 `internal/cli`（`agent plugins cache list|remove|prune`，因为只有它读得到 `plugins.json`，才知道哪些 digest 还被引用）。外加一处：验签失败时把那份包从缓存里清掉。

**Tech Stack:** Go 1.26.0，标准库。不引依赖。

**上游依据:** 路线 `plans/2026-08-28-plugin-gap-closure-roadmap.md` 的 G6；2026-08-28 真机走查记录的「422 后不可信的包永久留在缓存」。

## 前置事实（已在 master）

```go
// internal/plugin/fetch —— cache.go
type Cache struct{ root string; mu sync.Mutex; lockWait, lockPoll time.Duration }  // :97
func NewCache(root string) (*Cache, error)                                         // :140
func CacheRoot(root string) (string, error)                                        // :165
func (c *Cache) Dir(digest string) string        // panics on a malformed digest    // :196
func (c *Cache) Has(digest string) (bool, error) // "complete package present?"     // :219
func (c *Cache) Put(digest string, archive []byte, limits UnpackLimits) (string, error) // :269
func (c *Cache) lockDigestDir(dir string) (unlock func() error, err error)          // :461
func isCompletePackage(dir string) (bool, error) // plugin.json+wasm+sig all present // :507

// 目录形状：<root>/sha256/<64 hex>/{plugin.json,plugin.wasm,plugin.sig}
// 落盘中途的临时目录：<root>/sha256/.unpack-*      锁文件：<64hex>.lock
```

不可信包的检出点有两处，都在拿到目录之后调 `manifest.LoadPackage`：

- `internal/cli/plugin_consent_service.go:470`（`Resolve`，HTTP 422 的来源）
- `internal/plugin/loader/loader.go` 的 `prepare`（挂载路径）

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：删除失败要报，不得当作删过了。
- 公开标识符必须有 Go doc 注释。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。
- **每个 task 至少跑一次 `go test ./...`**；涉及并发的额外 `go test -race ./internal/plugin/...`。
- 每个 task 做变异验证：把核心机制改坏，确认测试确实 FAIL，输出留在报告里，然后还原并 `git status` 核对。
- 提交只 stage 本 task 的文件（显式路径），永不 `git add -A`。
- **Windows 提醒**：`internal/plugin/fetch` 的并发缓存测试在本机偶发 `Access is denied`（杀软/索引器碰 `.unpack-*` 与 `.lock`），是环境问题，重跑即绿——不要因此改锁逻辑。

## 三条不做

- **不做自动后台淘汰。** 一个正在跑的部署随时可能需要某份包（reload、resolve），后台线程按容量删包 = 把一次磁盘压力变成一次挂载失败。容量淘汰做成**显式命令**（`prune --max-bytes`）。
- **不做跨主机共享缓存**、不做周期性完整性重扫（路线里就排除了）。
- **不新增事件类型。** 缓存清除走 logger.Warn；`plugin/*` 那六个是运行时状态的契约，往里加「磁盘维护」是另一类东西。

---

### Task 1: `Cache.List` / `Cache.Remove`

**Files:**
- Modify: `internal/plugin/fetch/cache.go`
- Test: `internal/plugin/fetch/cache_governance_test.go`

**Interfaces:**
- Produces:
  - `type CacheEntry struct { Digest string; Bytes int64; ModTime time.Time; Complete bool }`
  - `func (c *Cache) List() ([]CacheEntry, error)`
  - `func (c *Cache) Remove(digest string) (removed bool, err error)`

**List 要报告不完整的条目，不能过滤掉**：一个只剩两个文件的目录既占磁盘又不会被 `Has` 认成命中，正是运维需要看见并清掉的东西。`Complete` 字段说明它是哪一种。

**Remove 必须拿与 `commit` 同一把 digest 锁**：另一个进程可能正在 `Put` 同一个 digest，删一个正在被写入的目录会把它变成半份。

**Remove 的返回值是 `(removed bool, err error)`**：删一个本来就不存在的 digest 不是错误（幂等），但也不能假装删掉了——`false` 是「本来就没有」这个事实，不是失败。

- [ ] **Step 1: 写失败测试**

新建 `internal/plugin/fetch/cache_governance_test.go`，夹具沿用 `cache_test.go` 现有的写法（**先读一遍**，那里已有构造 Cache 与写入包目录的助手，用它的，别新造平行的一套）：

```go
func TestCacheListReportsCompleteAndIncompleteEntries(t *testing.T) {
	// 一个完整包 + 一个只写了 plugin.json 的目录 →
	// List 返回两条，Complete 分别为 true/false，Bytes > 0。
}

func TestCacheListSkipsTempAndLockArtifacts(t *testing.T) {
	// .unpack-xxxx 目录与 <hex>.lock 文件都不是缓存条目，不许出现在 List 里
	// （否则运维会以为缓存里有一个叫 ".unpack-123" 的包）。
}

func TestCacheListOnAnEmptyCacheIsEmptyNotAnError(t *testing.T) {
	// 空缓存是正常状态，不是故障。
}

func TestCacheRemoveDeletesTheEntryAndReportsIt(t *testing.T) {
	// Put 一个包 → Has=true → Remove 返回 true → Has=false → 目录真的没了。
}

func TestCacheRemoveOfAnAbsentDigestIsFalseNotAnError(t *testing.T) {
	// 幂等，但要如实说「本来就没有」。
}

func TestCacheRemoveRefusesAMalformedDigest(t *testing.T) {
	// "sha256:zz.." / 没有前缀 / 路径穿越形状（"sha256:../../etc"）都必须报错，
	// 且**不能删除任何东西**——这是唯一一个会 os.RemoveAll 的入口，
	// 它接受的字符串必须先被证明是 64 位十六进制。
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/plugin/fetch/ -run "TestCacheList|TestCacheRemove" -v`
Expected: FAIL，`c.List undefined`

- [ ] **Step 3: 实现**

`List` 读 `<root>/sha256` 一层目录，跳过非目录与 `.` 开头的名字，用既有的 `isHexDigest` 判断是不是条目；`Bytes` 用 `filepath.WalkDir` 累加；`Complete` 复用 `isCompletePackage`。

`Remove` 先 `parseDigest`（复用既有解析，它就是防路径穿越的那道门），再拿 `lockDigestDir`，再 `os.RemoveAll`。

- [ ] **Step 4: 跑测试确认通过 + 全量 + race**

Run: `go test ./internal/plugin/fetch/ -run "TestCacheList|TestCacheRemove" -v` → PASS
Run: `go test ./...` → 全绿
Run: `go test -race ./internal/plugin/...` → 全绿

- [ ] **Step 5: 变异验证**

把 `Remove` 里的 `parseDigest` 校验去掉，确认 `TestCacheRemoveRefusesAMalformedDigest` FAIL；还原。

- [ ] **Step 6: 提交**

```bash
git add internal/plugin/fetch/cache.go internal/plugin/fetch/cache_governance_test.go
git commit -m "feat(plugin): 缓存可枚举、可删除（List/Remove）"
```

---

### Task 2: `agent plugins cache list|remove|prune`

**Files:**
- Modify: `internal/cli/plugins_command.go`（新增 `cache` 子命令树）
- Test: `internal/cli/plugins_cache_command_test.go`

**Interfaces:**
- Consumes: `fetch.Cache.List` / `Remove`（Task 1）
- Produces: `agent plugins cache list|remove <digest>|prune [--max-bytes N] [--dry-run]`

**策略住在 CLI，因为只有它读得到 `plugins.json`**：哪些 digest「还被引用」是部署清单的事实，`fetch` 不该知道清单的存在。

三个子命令的语义：

| 命令 | 做什么 | 不做什么 |
|---|---|---|
| `cache list` | 列出每条：digest、大小、修改时间、是否完整、**是否仍被清单引用** | — |
| `cache remove <digest>` | 删一条 | **不检查引用**：运维点名了就删（下次 reload 会重新下载）；但清单仍引用时**打 Warn 说明后果** |
| `cache prune` | 删掉**清单不再引用的**条目，外加超过 1 小时的 `.unpack-*` 残留 | 默认**不碰**仍被引用的条目 |
| `cache prune --max-bytes N` | 在上面的基础上，若总量仍超过 N，**按修改时间从旧到新**继续删，直到降到 N 以下 | 仍然**永不删仍被引用的条目**——降不下去就如实报告差多少 |

**`--max-bytes` 用修改时间而不是「最近使用时间」**，并且**必须在文档和帮助里说清**：这个仓不记录读取时间（记录它意味着每次缓存命中都要写盘），`ModTime` 是「最后一次写入/刷新」的时间。把它叫成 LRU 是撒谎。

**`--dry-run` 是必须的**，不是锦上添花：一条会删磁盘的命令要能先看它打算删什么。

- [ ] **Step 1: 写失败测试**

```go
func TestPluginsCacheListShowsReferencedAndUnreferencedEntries(t *testing.T) { … }
func TestPluginsCachePruneRemovesOnlyUnreferencedEntries(t *testing.T) {
	// 清单引用 A、不引用 B → prune 后 A 还在、B 没了。
}
func TestPluginsCachePruneDryRunDeletesNothing(t *testing.T) {
	// 输出里点名 B，但磁盘上 A、B 都还在。
}
func TestPluginsCachePruneMaxBytesNeverEvictsAReferencedEntry(t *testing.T) {
	// --max-bytes 小到必须删「仍被引用」的那条才能达标 → 它必须活着，
	// 命令如实报告"仍超出 N 字节"，退出码非零。
}
func TestPluginsCacheRemoveWarnsWhenTheEntryIsStillReferenced(t *testing.T) { … }
func TestPluginsCacheRefusesWhenNoCacheIsConfigured(t *testing.T) {
	// plugins.cache 为空 → 明确报"此部署未配置插件缓存目录"，
	// 而不是对着一个猜出来的路径操作。
}
```

夹具照 `plugins_command_test.go` 现有的写法（它已经有构造 config + plugins.json + 包目录的助手）。

- [ ] **Step 2-4: 红 → 实现 → 绿**

命令挂在既有的 `newPluginsCommand` 下（`cmd.AddCommand(newPluginsCacheCommand(out))`）。注意 `newPluginsCommand` 的分组注释里写着两组命令的区别——`cache` 属于「不碰 loader、只动磁盘」那组，**把它加进那段注释**，否则注释与代码当场就不一致了。

- [ ] **Step 5: 全量 + 变异**

Run: `go test ./...`

变异：把 prune 的「引用集」改成空集，确认
`TestPluginsCachePruneRemovesOnlyUnreferencedEntries` FAIL；还原。

- [ ] **Step 6: 提交**

```bash
git add internal/cli/plugins_command.go internal/cli/plugins_cache_command_test.go
git commit -m "feat(plugin): agent plugins cache list|remove|prune"
```

---

### Task 3: 验签失败的包立即移出缓存

**Files:**
- Modify: `internal/cli/plugin_consent_service.go`（`Resolve` 的 untrusted 分支）
- Modify: `internal/plugin/loader/loader.go`（`prepare` 的 `LoadPackage` 失败分支）
- Create: `internal/plugin/fetch/evict.go`（两处共用的小函数）
- Test: `internal/cli/plugin_consent_service_test.go`、`internal/plugin/loader/` 既有测试文件

**Interfaces:**
- Produces: `func EvictUntrusted(cache *Cache, digest string, logger *slog.Logger) `

今天一个验签失败的远程包会**永久留在缓存里**（2026-08-28 走查记录）：下次 `List` 把那一行报成 `load_failed`，界面也不再提供取回，于是那份不可信的字节就一直躺在部署可读的位置上。

**只在两个条件同时成立时清除**：

1. 失败链上有 `manifest.ErrUntrustedPackage`（其它失败——包损坏、缺文件、磁盘错——**不清**：那不是信任问题，删了只会让人下次重下一份同样坏的包）；
2. 这份包**来自缓存**（entry 是远程且带 digest）。本地目录的包不属于缓存，删掉就是删了运维自己的文件。

清除**必须打 Warn**（谁、哪个 digest、为什么），因为这是一次代理运维做出的删除。清除失败也打 Warn 并继续——原本的不可信错误才是要返回给调用方的那个，不能被一次删除失败盖掉。

- [ ] **Step 1: 写失败测试**

```go
func TestResolveEvictsAnUntrustedPackageFromTheCache(t *testing.T) {
	// 缓存里放一份签名坏掉的包 → Resolve 返回 untrusted → 缓存目录没了。
}
func TestResolveKeepsTheCacheWhenTheFailureIsNotATrustFailure(t *testing.T) {
	// 包损坏（缺 plugin.wasm）→ Resolve 失败 → 缓存目录仍在：
	// 那不是信任问题，删了只会让下次重下一份同样坏的包。
}
func TestResolveDoesNotTouchALocalPackageDirectory(t *testing.T) {
	// 本地 source 的 entry 验签失败 → 运维自己的目录必须原封不动。
}
```

- [ ] **Step 2-4: 红 → 实现 → 绿**

- [ ] **Step 5: 全量 + 变异**

变异：把「只在 untrusted 时清除」的判断去掉（改成任何 LoadPackage 失败都清），确认
`TestResolveKeepsTheCacheWhenTheFailureIsNotATrustFailure` FAIL；还原。

- [ ] **Step 6: 提交**

```bash
git add internal/plugin/fetch/evict.go internal/cli/plugin_consent_service.go internal/plugin/loader/loader.go internal/cli/plugin_consent_service_test.go
git commit -m "feat(plugin): 验签失败的包立即移出缓存"
```

---

### Task 4: 文档

**Files:**
- Modify: docs 仓 `agents/reference/reference-legion-agent-plugins-001.md`（§7.1 CLI 表加 `cache`、§9 排错更新那条「仓内无 eviction API」）
- Modify: docs 仓 `agents/reference/reference-legion-agent-cli-001.md`（`plugins` 段）
- Modify: docs 仓 `design/architecture/legion-plugin-system.md`（路线表 G6）

- [ ] **Step 1: 手册**

CLI 表加三行；§9 排错里那条「422 后不可信的包永久留在缓存；仓内无 eviction API，需手工清 `cache/sha256/<digest>`」**必须改**——它现在是过期的。改成：验签失败会自动清除；要手工看/清用 `agent plugins cache`。

写明 `--max-bytes` 用的是**修改时间**不是最近使用时间，以及为什么（不记录读取时间）。

- [ ] **Step 2: 路线表 G6 标记已交付**

- [ ] **Step 3: 提交（docs 仓单独分支与 PR）**

---

## 自检

**范围覆盖**：G6 的三件事——`cache list|remove|prune`（Task 2，机制在 Task 1）、验签失败立即移出缓存（Task 3）、容量上限（Task 2 的 `--max-bytes`，显式命令而非后台淘汰，理由见「三条不做」）——各有任务；文档在 Task 4。

**类型一致性**：`CacheEntry{Digest,Bytes,ModTime,Complete}` 在 Task 1 定义、Task 2 消费；`Remove(digest) (bool, error)` 在 Task 1 定义、Task 2 与 Task 3 都调用；`EvictUntrusted(cache, digest, logger)` 在 Task 3 定义并当场在两处使用。

**已知留白**：测试夹具名字以各文件现状为准（`cache_test.go` 与 `plugins_command_test.go` 都已有构造器）。这是**指向现有代码**，不是 placeholder。
