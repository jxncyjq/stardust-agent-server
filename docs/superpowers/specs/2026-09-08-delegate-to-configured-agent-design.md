# 按名字委派给已配置 agent（Spec B）

**日期**：2026-09-08
**状态**：设计已确认，待写实施计划

## 一、这是什么

Legion 今天有**两套互不相干的「agent」系统**：

- `AgentRuntimeResolver.ResolveTaskRunner`（`internal/runtime/agent_resolver.go:222`）是**真正的 per-agent 体系**——按 `task.AgentID` 查 `agentregistry`，取到该 agent 的 `Role` / `MaasProfile` / `ContextFiles` / `Workspace` / `Skills` / `DisabledTools`，校验 `DisabledTools` 里每个名字都在 `toolauth.GateableToolNames()`（`internal/toolauth/catalog.go:61`）里，按它的 profile 造推理客户端，然后建 Runtime。**但它只被 `coordinator.go` 调用**（顶层任务派发）。
- 委派路径（`delegate_task` → `RunSubTask` → `newSubRuntime` → `runChild`）**一次都没碰过 `agentregistry`**：`runChild`（`internal/runtime/delegation.go:239`）直接写死 `domain.Agent{ID: agentID, Role: "developer"}`，`agent_id` **只是个标签**。

而且 `agent_id` **根本不在 `delegate_task` 的 `InputSchema` properties 里**（只有 `goal` / `context` / `role` / `background` / `toolsets` / `tasks`），尽管 `delegateTaskArgs` 有 `AgentID` 字段（`delegation_tool.go:20`）、`handleDelegateTask` 也读 `call.Arguments["agent_id"]`（`:96`）。**代码读它，schema 里没有，模型发现不了。**

**结果：Legion 的子代理无法「按名字选中一个已配置的 agent」。** Spec B 补上这条。

## 二、边界与依赖

**依赖 Spec A**：本 spec 要改的 `delegateTaskDescriptor` / `handleDelegateTask` / `runChild` / 委派请求校验，正是 Spec A 大改过的地方。Spec A 在 [stardust-agent-server#159](https://github.com/jxncyjq/stardust-agent-server/pull/159) 上待合。**实施前必须先确定分支基点**（见文末）。

**本 spec 明确不做**：
- 不做后台子任务持久化（Spec C）
- 不改 `agentregistry` 的配置格式（不新增字段）
- 不新增 `list_agents` 工具——YAGNI，先把可委派的名字写进工具描述，等 agent 多到描述放不下再说
- 不动顶层任务派发（`coordinator.go`）那条既有路径

## 三、寻址与解析

### 3.1 `agent_id` 正式进 schema

把 `agent_id` 写进 `delegateTaskDescriptor()` 的 `InputSchema.properties`，描述里列出可委派的 agent 名。single 与 batch 两种模式都支持——batch 的每条 `{goal, context, role, toolsets, agent_id}` 各自指定。

### 3.2 解析规则

| 情形 | 行为 |
|---|---|
| 不传 `agent_id` | **与今天逐字相同**：克隆父 runtime，`Role: "developer"` |
| 传了、查得到 | 走 per-agent 装配（第五节） |
| 传了、查不到 | **硬拒**，错误点出是哪个名字，并列出可用名字（`agentregistry.Registry.Names()`，`registry.go:71`） |
| 传了、但解析器未接线（nil） | **硬拒** |

**绝不允许「传了但没生效」。** 解析器为 nil 时静默回落到克隆父，意味着「你以为选中了 browser-agent，其实跑的是父的克隆」——这正是本仓 fail-loud 铁律点名的静默降级，且本项目已因同类形状出过 Critical。

## 四、授权：目标 agent 自己说了算

### 4.1 含义

选中 agent 后，`Role` / `DisabledTools` / `MaasProfile` / `Workspace` / `Skills` / `ContextFiles` **全部按目标 agent 的配置**。

其中 `Role` 尤其实在：它今天被 `runChild` 写死成 `"developer"`，而 role 是喂给 `BatchRolePermissionEnforcer` 的（`internal/tool/builtin.go` 里有 `"developer:delegate_task": true` 这类条目）。改完之后，**目标 agent 的 role 决定它能调什么工具**。

### 4.2 那条提权通道——写进规格，不回避

受限 agent 可以委派给不受限 agent，做自己做不了的事。**这是本设计有意接受的性质**，不是疏漏。

它的宽度 = **运维在 agents 目录里配了什么**。理由：`internal/server/http.go:1102` 的 `AgentID: req.AgentID` 直接来自请求，**顶层调用方今天就能直接选中任何已配置 agent**。所以委派**没有创造新的可达能力**，改变的是**谁做这个选择**：从人变成模型。

### 4.3 据此三条要求

1. **只能选中已配置的 agent**，不能凭空构造。可选集合是人写的配置。
2. **每次具名委派必须落审计**：父 agent、目标 agent、goal。不是为了拦，是为了事后答得出「谁把活派给了谁」。
3. **`DisabledTools` 的既有校验照样跑**（每个名字必须是已知 gateable 工具）。复用 `ResolveTaskRunner` 会自动获得这条，但**必须有用例钉住**——否则就是「接缝在但没人测」。

### 4.4 工具集三层叠加

父传 `toolsets` 收窄、目标有 `DisabledTools`，**两者都生效、取交集**。

`toolsets` 是调用方对这一次委派的收窄意图，`DisabledTools` 是目标 agent 的固有限制，谁都不该抹掉谁。这不违反「目标说了算」：那条讲的是目标的配置**全部生效**，没说父的收窄要被丢掉。

## 五、构造路径

### 5.1 两个同名不同义的 role —— 必须分清

| 概念 | 取值 | 管什么 |
|---|---|---|
| `AgentConfig.Role`（`internal/agentregistry/config.go:7`） | `developer` 等 | **能调什么工具**（喂 `BatchRolePermissionEnforcer`），进 `domain.Agent` |
| `SubTaskSpec.Role`（`internal/runtime/delegation.go`） | `leaf` / `orchestrator` | **能不能再委派**，进子 runtime 的委派能力 |

**两者各行其道，各自有用例，且要有一条变异证明它们没被接串。** 混为一谈就是一次「名字对上了但语义不对」。

### 5.2 委派上下文必须由委派侧决定

`ResolveTaskRunner` 造的 Runtime 是给**顶层任务**用的——`depth` 出生即 0。直接拿它当子代理，**递归深度防护就没了**（今天靠 `depth+1` 与 `maxSpawnDepth`，且 `leaf` 根本不注册 `delegate_task`）。

解析出来的 runtime **必须**带上：父的 `depth+1`、父的 `maxSpawnDepth`、本次委派的 `leaf`/`orchestrator`。

现有 `TaskRunner` 接口只有 `RunTask(context.Context, domain.Agent, domain.Task) (domain.TaskRun, error)`（`internal/runtime/coordinator.go:37-39`），**传不进这些**。所以需要一条新的接缝——给 resolver 加一个带委派上下文的解析方法，或让它返回可继续配置的 runtime。**具体形状留给实施计划，但这条约束是硬的：子代理绝不能以 `depth` 0 出生。**

### 5.3 复用而非另起

走 `ResolveTaskRunner` 的装配逻辑，**不要另写一条 per-agent 装配路径**。理由与本仓一条既有裁决同源：**各写一遍就是第二套真相**。同一条不变量在插件那期被三条不同通道各绕过一次，最后靠收口到一个共用判定才终结。

## 六、失败模式：全部硬拒，不静默回落

- `agent_id` 查不到 → 拒，列出可用名字
- 解析器未接线而传了 `agent_id` → 拒（3.2 已述，**最该防的一条**）
- 目标的 `DisabledTools` 含未知 gateable 名字 → `ResolveTaskRunner` 已会报错，**那个错误必须原样传出来**，不能被委派层吞掉
- 目标的 `MaasProfile` 造不出推理客户端 → 拒

错误一律 `fmt.Errorf("...: %w", err)` 包装，**错误点必须可定位**：点出是哪个 `agent_id`、批量里第几条。

## 七、测试

本仓最该防的形状是「**接缝在，但没人测那条接缝**」——上一期复发八次，Spec A 期间又复发多次（batch 出口、async 入口接线、`max_rounds` 话术臂、常量字面值、nil 注册表分支），**每一次都靠变异实证抓出来，静态审查一次都没抓住**。

**每条规则都要变异实证**：把它在实现里失效掉（**`go vet` 必须干净，不能靠编译失败**），确认对应用例 FAIL，还原确认 PASS。**哪条变异之后测试仍然全绿，那条规则就没人守着，必须补测试而不是放过。**

必须有用例守的接缝：

1. **默认路径没被碰坏**：不传 `agent_id` → 行为与改动前逐字相同（克隆父 + `Role: "developer"`）
2. `agent_id` 查不到 → 拒，且错误里**列出了可用名字**
3. **解析器 nil 而传了 `agent_id` → 拒**，不是回落
4. 选中的 agent 的 `Role` **真的进了** `domain.Agent`（不再是写死的 `"developer"`）
5. 选中的 agent 的 `DisabledTools` **真的生效**（那个工具确实调不动）
6. **子代理不以 `depth` 0 出生**：变异——让解析路径不带 `depth+1` → 必须有用例红（否则递归防护静默失效）
7. **两个 role 没被接串**：变异——把 `AgentConfig.Role` 喂给委派能力、或把 `SubTaskSpec.Role` 喂给权限判定 → 必须红
8. **三层叠加取交集**：父 `toolsets` ∩ 目标 `DisabledTools`，两侧各有一条变异（只丢掉父的收窄 / 只丢掉目标的限制）
9. **具名委派落了审计**（父 agent、目标 agent、goal）
10. 目标 `DisabledTools` 含未知 gateable 名字 → 错误**原样传出**，没被吞
11. batch 模式每条各自的 `agent_id` 都生效（不是只认第一条）——Spec A 期间 batch 出口漏测过一次

## 八、全局约束

- **fail-loud 铁律**（本仓 CLAUDE.md 最高优先级）：不许回落零值、不许吞错误、不许静默跳过、不许「拿不到就当没配置」。错误用 `fmt.Errorf("...: %w", err)` 包装，**错误点必须可定位**；错误被吞咽 / 终止的边界必须结构化记录。
- **注释是契约**：注释里每条事实陈述必须与代码一致，且**不得出现关于「谁调用它 / 有没有调用方 / 某个东西落没落地」的句子**。凡是提到别的文件 / 常量 / 包行为的句子，**亲自去那个文件确认再写**。**写封闭枚举（"Only …" / "Two …"）之前先把所有出口数一遍**——Spec A 期间在这上面栽过三次。
- **不得引入包级可变量**（包级函数不受此限）。
- **加新工具必须登记 `toolauth` gateable**（本仓既有教训：三件套漏登记 = 授权绕过）。本 spec 不新增工具，但若实施中新增，此条适用。
- 全绿判据：`go test ./... -count=1 -p 1 -timeout 900s`；`go test ./internal/runtime/ -race -count=1`；`gofmt -l .` 为空；`go vet ./...` 干净。**测退出码不要接管道**（管道会让 `$?` 取到 `tail` 的状态）。
- 只按**显式路径** `git add`，**不用 `git add -A`**。提交信息正文中文，结尾一行 `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`。

## 九、涉及文件（预估）

| 文件 | 改动 |
|---|---|
| `internal/runtime/delegation_tool.go` | `agent_id` 进 `InputSchema`；描述里列出可委派名字 |
| `internal/runtime/delegation.go` | `runChild` 不再写死 `Role: "developer"`；具名委派走 per-agent 装配；审计 |
| `internal/runtime/agent_resolver.go` | 新增带委派上下文（`depth+1` / `maxSpawnDepth` / `leaf`\|`orchestrator`）的解析接缝 |
| `internal/runtime/coordinator.go` | `TaskRunner` / `TaskRunnerResolver` 接口若需扩展 |
| Runtime 装配处 | 把 resolver 接给委派路径（今天 Runtime 不持有它） |

## 十、实施前必须先定的一件事：分支基点

Spec A（PR #159）**尚未合并**，而它大改过本 spec 要动的同一批函数（`delegateTaskDescriptor` / `handleDelegateTask` / `runChild`，并新增了 `validateSubTaskSpec` 与批量整批预检）。

三条路：

1. **先合 A，再从 master 开 B**（推荐）——无冲突、无 stacked PR。
2. 从 A 的分支开 B——stacked PR。本仓吃过亏：下层 squash 合并并删分支后，上层 PR 会被 GitHub **自动关闭且无法重开或改 base**，必须 `rebase --onto origin/master` 后新开 PR。
3. 从 master 开 B——B 要在「没有 `validateSubTaskSpec`」的前提下实现校验，A 合并时**大概率严重冲突**。

**这条要人拍板，不能由实施者自行决定。**
