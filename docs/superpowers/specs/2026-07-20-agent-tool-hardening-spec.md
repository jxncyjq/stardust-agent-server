# Agent 工具层加固 Spec

状态：**已全部实施并合并**（PR-A ~ PR-E，见 §11）
日期：2026-07-20
范围：`create_task` / `update_task` / `read_task` / `rebuild_tasks` / `claim_task` / `append_task_message` / `read_file` / `list_files` / `search_content` / `send_message` / `read_messages` / `fetch_url`

---

## 0. 证据可信度声明

本 spec 的结论分两类，**不要混用**：

| 标记 | 含义 |
|---|---|
| ✅ **已复核** | 我本人读过对应源码并确认，行号可直接跳转 |
| ⚠️ **待复现** | 来自专项审查，我未逐条验证，**动手修之前应先写失败测试复现** |

所有 P0 项均为 ✅ 已复核。

---

## 1. 优先级总览

| # | 问题 | 等级 | 状态 | 建议 PR |
|---|---|---|---|---|
| P0-1 | `read_messages{}` 可读全库消息，零租户隔离 | Critical | ✅ | PR-A |
| P0-2 | 非法 `task_id` 可永久损坏账本（毒丸） | Critical | ✅ | PR-B |
| P0-3 | 账本读事件静默吞错 → 投影被残缺数据覆写 | Critical | ⚠️ | PR-B |
| P1-1 | `claim_task` 无互斥，双方均报成功 | High | ✅ | PR-C |
| P1-2 | symlink 可逃逸文件沙箱 | High | ✅ | PR-D |
| P1-3 | 账本锁无 stale 回收，crash 后永久死锁 | High | ⚠️ | PR-C |
| P2-1 | `filepath.Abs` 失败静默降级，沙箱根失效 | Medium | ✅ | PR-D |
| P2-2 | `search_content` 无大小/条数上限 | Medium | ⚠️ | PR-E |
| P2-3 | `status` 为不受控自由字符串 | Medium | ⚠️ | PR-B |
| P2-4 | 其余 fail-loud 违规（约 15 处） | Medium | 部分 ✅ | 分散（另见 §11 后续批次 #29/#30/#32） |

---

## 2. P0-1：消息工具零租户隔离

### 现状（✅ 已复核）

**`internal/storage/sqlite.go:641-657`** —— 所有过滤条件都是「参数为空则恒真」：

```sql
WHERE (? = '' OR company_id = ?)
  AND (? = '' OR to_agent_id = ?)
  ... LIMIT ?          -- limit<=0 时默认 100
```

**`internal/tool/registry.go:165`** —— `Execute` 的作用域内**有** `agent` 变量（同函数后续 `r.appendAudit(ctx, agent, call, ...)` 在用），但没有传给 handler：

```go
result, err := handler.Execute(execCtx, call)   // handler 拿不到调用者身份
```

### 后果

任一 sub-agent 调用 `read_messages{}`（不带参数）即可读到**全库**前 100 条消息，跨 agent、跨 company。`company_id` 目前只是模型自填的普通工具参数，**服务端无任何强制**。

### 修复方案

分两层，缺一不可：

**第一层：让 handler 知道调用者是谁**

`ToolHandler` 接口增加调用者身份。两种做法二选一，建议 (a)：

- **(a) 扩展接口签名**：`Execute(ctx, agent domain.Agent, call domain.ToolCall)`。改动面大但显式，编译器保证每个 handler 都拿到身份。
- (b) 经 context 注入：`ctx = tool.WithCaller(ctx, agent)`。改动小，但 handler 可以忘记取，且类型不安全。

**第二层：在消息 handler 内强制覆盖过滤条件**

`internal/tool/agent_message.go` 的 `read_messages`：

- `ToAgentID` **强制**设为调用者的 agent ID，忽略模型传入的值；
- `CompanyID` 同理，取自调用者而非参数；
- 模型若传了不同的 `to_agent_id`，**返回 error**（fail-loud），而不是静默改写 —— 让越权尝试可见。

`send_message` 的 `FromAgentID` 同样强制取自调用者，禁止伪造发件人。

**第三层（可选，纵深防御）**：`ListAgentMessages` 在 `CompanyID` 与 `ToAgentID` 同时为空时返回 error 而非全表扫描。理由：这个仓储方法没有任何合法场景需要「查全部租户的消息」，把它做成不可能比依赖调用方自觉更可靠。

### 验证

必须先有失败测试：

1. 无参数 `read_messages{}` → 只返回调用者自己的消息（修复前返回全库）
2. `read_messages{to_agent_id: "别人"}` → 返回 error
3. `send_message{from_agent_id: "伪造"}` → 发件人被强制为真实调用者，或返回 error
4. 跨 company 读取 → 返回空或 error

---

## 3. P0-2：非法 task_id 永久损坏账本

### 现状（✅ 已复核）

**`internal/taskledger/ledger.go:220-247`** `validateEvent` 只检查 `TaskID != ""`，**不校验格式**。同一函数内 `Artifact` 反而做了 `resolveWithin` 路径校验 —— 明显的疏漏而非有意设计。

**`ledger.go:344`** `taskPath` 才拒绝含分隔符的 task_id：

```go
if strings.Contains(taskID, string(filepath.Separator)) ||
   strings.Contains(taskID, "/") || strings.Contains(taskID, "\\") {
    return "", fmt.Errorf("%w: unsafe task_id %q", ErrInvalidEvent, taskID)
}
```

但它在**投影写入阶段**才被调用。

### 后果（不可逆）

`create_task{task_id: "../evil"}`：

1. `Append` → `validateEvent` 放行 → 事件**已落盘**到 JSONL
2. 随后 `Rebuild` → `taskPath` 报错 → 整个 Rebuild 失败
3. 此后**每一次** Rebuild 都会重放到这条坏事件并失败
4. 账本整体不可用，且**没有任何工具能删除已落盘的事件**

只能人工编辑 JSONL 文件恢复。

### 修复方案

**把校验前移到写入路径**：`validateEvent` 增加 `task_id` 格式校验，复用 `taskPath` 的同一套规则。

关键要求：**两处必须共用同一个函数**，不能各写一份——否则又是一次「判定规则漂移」（本仓库刚在 skills 根目录判定上踩过完全相同的坑，见 PR #23）。

建议提取：

```go
// ValidateTaskID 是 task_id 的唯一合法性闸门。写入路径与投影路径共用它，
// 避免两处规则漂移导致「写得进、重建不出来」的不可恢复状态。
func ValidateTaskID(taskID string) error
```

`validateEvent` 与 `taskPath` 都调用它。

**建议同时约束的内容**（需你确认，见 §9 开放问题）：
- 禁止路径分隔符 `/` `\`（必须）
- 禁止 `..`（必须）
- 禁止空白字符、控制字符
- 是否限定字符集（如 `[A-Za-z0-9_-]`）与长度上限

**补充：坏数据的逃生舱**。即使前移了校验，已落盘的坏事件仍需要能清理。建议评估：Rebuild 遇到无法投影的事件时，是整体失败（现状），还是跳过并**大声报告**（记 Error 日志 + 在投影中列出被跳过的事件）。后者更符合「可运维」，但要小心不能变成静默忽略。

### 验证

1. `create_task{task_id: "../evil"}` → **Append 阶段**即返回 error，事件**不落盘**
2. 同上，`"a/b"`、`"a\\b"`、`".."`、`" "` 均被拒
3. 合法 task_id 不受影响（回归）
4. 变异测试：把 `validateEvent` 的新校验删掉，上述测试必须失败

---

## 4. P0-3：读事件静默吞错 → 投影被残缺数据覆写

### 现状（⚠️ 待复现）

`internal/taskledger/ledger.go:176-178` 附近：

```go
if walkErr != nil { return nil }    // 吞掉遍历错误
```

任一事件文件不可读（权限、损坏、被占用）→ `ReadEvents` 返回**残缺**事件集 → `Rebuild` 用它重建并**原子覆写** `tasks.md` 与全部 `tasks/*.md`。

### 后果

历史任务被静默抹除，无 error 无日志。这是**写路径**上的兜底，直接违反 CLAUDE.md 第 0 节。

### 修复方案

遍历出错立即返回 error 并用 `%w` 包装文件路径。`Rebuild` 拿到 error 后**不得写投影**——宁可保持旧投影，也不能用残缺数据覆盖。

### 验证

构造一个不可读的事件文件（chmod 000 / Windows 上用占用句柄），断言 `Rebuild` 返回 error 且 `tasks.md` **内容未变**。

---

## 5. P1-1：claim_task 无互斥

### 现状（✅ 已复核）

`Append`（`ledger.go:98-130`）有 `sync.Mutex` + 进程间 `.lock` 文件，但那保护的是**事件写入的原子性**，不是业务约束——`Append` **不读当前 owner**。

`ErrDuplicateClaim`（`ledger.go:20`）声明后**全仓库零引用**，强制校验从未实现。

### 后果

两个 agent 同时 `claim_task` 同一任务：两条 `task.claimed` 都落盘，**两边都收到 `Success: true`**。冲突仅在投影阶段记为 Markdown 里一行 `conflict.owner_claim`。

调用方无法从返回值判断自己是否真的拿到任务——这是事后记录，不是互斥。多 agent 并发协作场景下会导致重复执行同一任务。

### 修复方案

在 `Append` 持锁期间读取该任务当前 owner，已被他人持有时返回 `ErrDuplicateClaim`，工具层将其映射为失败结果（`Success: false` + 明确原因）。

注意：这要求 `Append` 能在持锁状态下查到当前 owner。若代价过高（需全量重放），可考虑维护一个轻量的 owner 索引，但需评估与事件溯源模型的一致性——**这一点建议在实施前单独讨论**。

### 验证

并发测试：N 个 goroutine 同时 claim 同一 task_id，断言**恰好一个**成功、其余返回 `ErrDuplicateClaim`。

---

## 6. P1-2：symlink 逃逸文件沙箱

### 现状（✅ 已复核）

`internal/port/path_guard.go` 是**纯词法校验**：

```go
clean := filepath.Clean(path)
rel, err := filepath.Rel(g.root, clean)
if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) { /* 拒绝 */ }
```

全仓库 grep `EvalSymlinks` / `Lstat` / `ModeSymlink` **零命中**——不解析符号链接。词法检查通过后 `os.Open` / `WalkDir` 自然跟随链接。

**其余穿越手法均被正确拦截**（已复核）：`../`、绝对路径、Windows 跨盘符与 UNC（`filepath.Rel` 对异盘符直接返回 error）。

### 后果

工作区内存在 `link -> C:\Users\x\.ssh`，则 `read_file{path:"link/id_rsa"}` 读出沙箱外文件。

**前提**：需要沙箱内**已存在**指向外部的链接。攻击者无法通过现有工具创建链接（`write_file` 写文件不建链接），所以这是「利用既有链接」而非「凭空构造」。真实项目目录里存在 symlink 并不罕见。

### 修复方案

`WorkspacePathGuard.Check` 在词法校验**之后**追加真实路径校验：

1. 对目标路径做 `filepath.EvalSymlinks`
2. 对 root 同样做 `EvalSymlinks`（root 自身可能就在符号链接下）
3. 用解析后的两个真实路径重新做 `Rel` 判定

边界处理需明确：**目标尚不存在时** `EvalSymlinks` 会失败（写文件场景），此时应对其**最近的已存在祖先**做解析再判定，不能因为文件不存在就跳过校验。

`WalkDir` 遍历（`list_files` / `search_content`）需单独处理：默认不跟随目录符号链接，或对每个条目重新过闸。

### 验证

1. 沙箱内建 symlink 指向外部文件 → `read_file` 返回 `ErrPathOutsideWorkspace`
2. 沙箱内建 symlink 指向**沙箱内**文件 → 正常读取（不能误杀）
3. root 本身位于 symlink 下 → 沙箱内正常文件仍可读
4. `list_files` / `search_content` 不跟随外链
5. 目标文件不存在时校验仍生效

> Windows 上创建 symlink 需要管理员权限或开发者模式；测试应在不支持时 `t.Skip` 并说明，不要静默通过。

---

## 7. P2 项

### P2-1：`filepath.Abs` 失败静默降级（✅ 已复核）

`internal/tool/builtin.go:49-51` 与 `108-111`：

```go
absRoot, err := filepath.Abs(root)
if err != nil {
    absRoot = root      // 沙箱根退化为相对路径
}
```

`filepath.Abs` 仅在 `os.Getwd()` 失败时出错，罕见——但一旦发生，`NewWorkspacePathGuard` 拿到相对路径，沙箱边界的比较语义不再成立。**这是安全边界上的静默降级**。

**修复**：构造函数改为返回 error，或在此处 `panic`（属于启动期不变量违反，符合 CLAUDE.md 对「不可恢复的编程错误」的处理约定）。建议前者。

### P2-2：`search_content` 资源无上限（⚠️ 待复现）

每个文件 `os.ReadFile` 全量读入内存、无单文件上限、不响应 ctx；matches / entries 无条数上限，输出可达数百 MB 并整体进入 LLM 上下文。

**修复**：单文件大小上限（复用 `read_file` 的 256KiB 或单独配置）+ 结果条数上限 + 截断时**显式标注**已截断（不可静默丢弃）。

> 附带澄清：`search_content` 用的是 `strings.Contains` 而非正则，**不存在 ReDoS**。这一点纠正了我最初的假设。

### P2-3：`status` 不受控（⚠️ 待复现）

`status` 是完全自由的字符串，`Config.ActiveStatuses` / `DoneStatuses` 从未被消费。写入 `"donee"` 的任务永不归档。

**修复**：`validateEvent` 校验 status 属于配置的枚举集合，非法值返回 error。

### P2-4：其余 fail-loud 违规

已定位约 15 处，含吞 `walkErr`、吞 `ReadFile` 错误、`"no matches"` 冒充成功、`Rel` 出错回退绝对路径、锁释放失败被吞（`os.Remove(lockPath)`）等。

建议**不单独开 PR**，随各自模块的修复一并处理，避免大范围散弹式改动。

---

## 8. 建议的 PR 拆分

按风险与独立性排序，**逐个合并、逐个验证**：

| PR | 内容 | 依赖 | 规模 |
|---|---|---|---|
| **PR-A** | 消息租户隔离（P0-1） | 无 | 中（改 handler 接口） |
| **PR-B** | 账本写入校验前移 + 读错误不吞（P0-2、P0-3、P2-3） | 无 | 中 |
| **PR-C** | claim 互斥 + 锁 stale 回收（P1-1、P1-3） | PR-B 之后更稳 | 中 |
| **PR-D** | symlink 校验 + `Abs` fail-fast（P1-2、P2-1） | 无 | 小到中 |
| **PR-E** | `search_content` 资源上限（P2-2） | 无 | 小 |

PR-A 与 PR-D 可并行；PR-B → PR-C 建议串行。

每个 PR 遵循本仓库既有做法：**先写失败测试复现，再修，并对新测试做变异验证**（把修复改回旧写法，确认测试确实失败）。

---

## 9. 需要你决策的开放问题

1. **`task_id` 字符集**：是否限定为 `[A-Za-z0-9_-]` + 长度上限？收紧会拒绝历史上已存在的 task_id 吗？（现有账本里的 ID 形如 `TASK-20260523-101`，符合该字符集）
task_id不变，但这个id内部使用，不显示出来，可用于查日志，做调试使用

2. **已落盘坏事件的处理**：Rebuild 遇到无法投影的事件，整体失败（现状）还是跳过并大声报告？
直接报告出来 

3. **`claim` 互斥的实现代价**：持锁读 owner 是否需要引入 owner 索引？还是每次全量重放可接受（取决于账本规模）？
需要引入owner索引

4. **`fetch_url` 默认策略**：当前 `Enabled: true` 且 `Allowlist` 为空（允许任意公网域名）。抓取内容直接进模型上下文，是间接提示注入入口。是否改为默认白名单模式？——**这是产品决策，不是缺陷**。
不需要白名单确认

5. **`NewReadOnlyWorkspaceRegistry` 更名**：其「只读」仅指文件系统只读，调用方随后仍会注册 `create_task` / `send_message` / `fetch_url` 等有副作用的工具。是否更名（如 `NewFileReadOnlyWorkspaceRegistry`）以免误导？
赞同更名

---

## 10. 不在本 spec 范围

- `fetch_url` 的 SSRF 防护经复核**是完善的**：scheme 白名单、`checkURLHostAllowed` 预检、`dialer.Control` 在建连时校验**实际解析出的 IP**（因此 DNS rebinding 不成立）、`CheckRedirect` 复检私网与 allowlist、`io.LimitReader` 512KB、超时 20s。**无需修改**。
- `read_file` 已有 256KiB 上限，无需变更。
- 遍历超时机制（5s）工作正常。

---

## 11. 实施记录（2026-07-20）

五个 PR 全部完成并合并到 `master`：

| PR | 内容 | spec 项 |
|---|---|---|
| #24 | 消息工具强制按调用者隔离 | P0-1 |
| #25 | 账本校验前移 + 读错误不吞 + status 枚举 | P0-2 / P0-3 / P2-3 |
| #26 | claim 独占化 + 回收陈旧锁 | P1-1 / P1-3 |
| #27 | symlink 不再逃逸沙箱 + `Abs` fail-fast + 注册表更名 | P1-2 / P2-1 |
| #28 | `search_content` / `list_files` 资源上限，截断必声明 | P2-2 |

每个 PR 均为：先写失败测试复现 → 修复 → 变异验证（把修复改回旧写法，确认测试确实失败）→ CI 绿 → 合并。

### 三处对本 spec 的偏离（已在各 PR 中说明）

1. **P0-1 的身份传递**：spec §2 建议扩展 `Handler.Execute` 签名，实施时统计改动面为 **77 处**（生产 17 + 测试约 60）。改用 context 注入 + `RequireCaller`（取不到身份即拒绝执行），避免把安全修复摊成大规模机械改动。
2. **P2-1 的 `Abs` 处理**：spec §7 建议构造函数返回 error，实施时统计构造函数共 **27 处**调用点。改用 `panic` —— `filepath.Abs` 仅在 `os.Getwd` 失败时出错，属不可恢复的环境故障，符合 `CLAUDE.md` 对此类情形的既定约定。
3. **P2-3 的 status 校验**：只在写入路径生效，不参与历史事件重放，避免让既有账本失效。

### race detector 验证状态（更正 PR #26 的说明）

PR #26 的描述中写有「本机无 gcc，`-race` 无法执行，故并发正确性仅由行为测试保证，没有 race detector 证据」。**该说法已过时，此处更正**：

合并后已在两套环境完成全量 `go test -race ./...`，**均零数据竞争、零失败**：

- **Windows + LLVM-MinGW**（clang 22.1.8，target `x86_64-w64-windows-gnu`）
- **WSL Ubuntu 22.04 + `/opt/go`**

当时判断"无 gcc"的原因是：shell 会话启动早于 gcc 安装，进程继承的是旧 `PATH`，而 gcc 其时已在系统 PATH 中。

WSL 那一轮还额外验证了两组在 Windows 上无法真实执行的测试：

- `TestReadEventsFailsLoudOnUnreadableDirectory`（依赖 `chmod 000`，Windows 上 `t.Skip`）—— **P0-3 的修复此前没有任何自动化覆盖，至此才被真正验证**
- `internal/port` 的 symlink 系列（Linux 原生执行，无需开发者模式）

具体命令与两个环境陷阱见 [`docs/agents/reference/testing.md`](../../agents/reference/testing.md)。

### 后续批次（本 spec 之外，但同属 fail-loud 收敛）

spec 定稿后又合入三个 PR，P2-4 由此大幅收敛：

| PR | 内容 |
|---|---|
| #29 | 审计写入、日志构造、账本根路径三处不再静默吞错 |
| #30 | 身份校验改由 `require_identity` 开关显式声明 |
| #32 | `port.EventBus.Events` / `port.AuditLog.Events` 返回 error——事件与审计**读取**侧不再把后端故障伪装成「还没有事件」，与 #29 的写入侧互补 |

#32 来自一个基于 47 个提交之前的分支，合并时生产代码编译干净，但三处测试需适配（含两个语义相反的 `failingAuditLog` 同名冲突，已按失败方向重命名为 `writeFailingAuditLog` / `readFailingAuditLog`）。

### 尚未处理

- **P2-4 剩余部分没有已知清单**。原记录的行号早已漂移，机械 grep 会命中大量正常路径的合法返回，计数无参考价值。按 §7 的建议随各模块修复一并处理，判据是 CLAUDE.md 第 0 节：**可选 = 契约允许它不存在；兜底 = 出错了却假装没事**。
- §9 开放问题 3 提到的「Rebuild 遇坏事件整体失败还是跳过并报告」已按决策实现为**跳过并报告**，诊断信息渲染进 `tasks.md` 顶部。
