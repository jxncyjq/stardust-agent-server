# 能力目录与按需加载（Capability Catalog）设计

日期：2026-07-21
状态：已定稿，待实施
基线：`legionAgent` master `2876d96`

## 1. 目标与非目标

### 目标

1. **技能不再靠关键词猜**。当前 `cognitive/core.go:238` 用 `SelectForTask(task, 3)` 按 `task.Input` 关键词打分，取前 3 个注入；`Summary` 为空时注入的是**技能全文**（`core.go:255`）。改为：全量技能以「名字 + 一行」进入 system prompt，模型自己决定拉哪份正文。
2. **为工具规模增长准备好机制**。工具来源将扩展到 MCP、per-agent、插件三类，数量不可控。建立目录 + 按需加载协议，在规模失控时自动接管。
3. **不牺牲 prompt 缓存**。目录进入稳定前缀，利用仓库已有的 `cache_control` 能力，使重发成本按缓存价计。

### 非目标

- **不做**「加载后把工具提升为原生 `tools` 数组项」。理由见 §5.3。
- **不做** MCP 接入本身。本设计只保证 MCP 工具接入时协议不必改。
- **不做** 工具 schema 的压缩/精简（如 opencode 的 `json-schema.ts::normalize()`）。这是与本设计正交的另一条优化，可独立立项。

## 2. 现状测量（本设计的事实基础）

以下数据由一次性测量得出（测量代码未入库）。序列化口径为 `port.InferenceTool{Name, Description, InputSchema}` 的 JSON 字节数。

| 装配路径 | 工具数 | 字节 |
|---|---|---|
| serve 默认任务 | 15 | 10 029 |
| CLI `run`/`tui` | 13 | 7 601 |
| per-agent | 12 | 6 960 |
| Plan 模式子集 | 6 | 3 630 |
| 现有 lazy 协议的 2 个 meta 工具 | 2 | 910 |

最肥的三个工具，肥在 schema 而非描述：`send_message`（描述 44 B / schema 928 B）、`append_task_message`（56 / 782）、`read_messages`（40 / 738）。

## 3. 先例调研

调研了 4 个代码库（第 5 个 DeepSeek-TUI 因子 agent 反复失败未取得数据）。

| 项目 | 工具数 | 工具懒加载 | 技能目录进 prompt | 技能正文按需 |
|---|---|---|---|---|
| hermes-agent | 72 内置 + MCP | ✅ 真做，阈值门控 | ✅ 按 category 分组 | ✅ 三层披露 |
| opencode | 13 内置 + MCP | ❌ 每轮全量发 | ✅ `<available_skills>` | ✅ `skill` 工具 |
| claw-code | 60 内置 | ⚠️ **有壳无实** | ❌ 缺失 | ✅ `skill` 工具 |
| evolver | 不适用 | 不适用 | 不适用 | 不适用 |

四条可直接采用的结论：

1. **三元工具协议**（hermes `tool_search` / `tool_describe` / `tool_call`）是这个问题的收敛形态。
2. **核心工具永不延迟**。hermes 的 `_HERMES_CORE_TOOLS`（约 45 个）始终全量下发，只延迟 MCP 与非核心插件工具；claw-code 同样划出 6 个核心文件/命令工具。
3. **阈值门控**。hermes 默认 `auto`：可延迟工具的 schema 预估 ≥ 上下文窗口 10%（窗口未知时退化为 20K token 固定门槛）才激活。小工具集下激活是净亏。
4. **技能：目录推送 + 正文按需**。三家一致，无一例外。claw-code 是反面教材 —— 它的技能目录**不进 system prompt**，模型必须已经知道技能名才能调用，等于技能不可发现。

两条反面教训：

- **claw-code**：有 `ToolSearch` 元工具、有 `deferred_tool_specs()` 分类，但每轮请求照样发送全部 60+ 工具的完整 schema。协议长得像懒加载，一个 token 没省。**验收必须断言 `tools` 数组体积确实变小。**
- **hermes**：`tool_call` 会拒绝不在当前会话 toolset 内的工具，有专门测试 `test_tool_call_rejects_out_of_scope_tool`。桥接工具若不做作用域检查，就是权限绕过通道。

一条**无先例**的决策：工具与技能统一到同一抽象。四家都是各走各的两套机制。本设计选择做一个**瘦**的统一层（只统一条目形状与明细获取，不统一投递），理由见 §4.1。

## 4. 架构

### 4.1 `internal/capability`（新包，只读，不执行）

```go
type Kind uint8            // KindTool | KindSkill
type Entry struct {
    Name    string
    Group   string
    Summary string
    Kind    Kind
}

type Provider interface {
    Entries(ctx context.Context) ([]Entry, error)
    Detail(ctx context.Context, name string) (string, error)
}

type Catalog struct{ providers []Provider }
```

两个 provider：

- `ToolProvider` 包 `*tool.Registry`：`Entries` 取自 `Descriptors()`，`Detail` 返回 `{name, description, input_schema}` 的 JSON。
- `SkillProvider` 包 `*skill.System`：`Entries` 取自 `Load()`，`Detail` 返回技能正文。

**只统一两件事**：条目形状（名字 + 分组 + 一行）与明细按需获取。**不统一投递** —— 因为工具有原生形态而技能没有：

| | 常驻形态 | 明细获取 |
|---|---|---|
| 核心工具 | 原生 `tools` 数组（完整 schema） | 不需要 |
| 可延迟工具 | 目录条目 | `load_capabilities` |
| 技能 | 目录条目（技能无原生形态，不推则不可发现） | `load_capabilities` |

### 4.2 三条边界

1. **`capability` 不执行任何东西**。`call_tool` 仍直接走 `tool.Registry.Execute`，权限、审计、超时、sanitizer、Manual gate 一条都不绕过。技能的「执行」就是 `Detail` 返回正文，没有第二步。
2. **技能不进 `tool.Registry`**。Manual gate 遍历 registry 的 `Sensitive` 字段做审批决策（`internal/manualgate/manualgate.go:80`），塞入不可执行的条目会污染该语义。
3. **目录无状态、每次重建**。采纳 hermes 的明确理由：避免目录与真实 registry 漂移导致工具悄悄消失。技能侧因需扫盘，允许带 mtime/size 校验的进程内缓存（见 §6.1）。

### 4.3 `lazytools.go` 重构

该文件当前 249 行，同时承担协议分发、目录渲染、参数解析、事件发布四项职责。目录渲染移入 `capability`，参数解析独立成文件，`lazytools.go` 回到只管协议分发。

## 5. 会话态与生命周期

### 5.1 已加载能力靠对话历史留存

`load_capabilities` 的返回值**只是一句确认**（`loaded: a, b`），正文进入 run state 的独立**已加载区块**。这样做同时解决三个问题：

- 不受 `maxToolResultChars = 4000`（`internal/runtime/runtime.go:109`）截断 —— 否则大 schema 会被切成非法 JSON；
- 不进 `toolCtx`，因而不参与中段丢弃（见 §5.2）；
- 天然幂等，重复加载同名能力是覆盖而非累积。

已加载区块随 checkpoint 持久化，因此挂起审批、进程重启后仍在（现有 `toolCtx` 已有同款机制：`runtime.go:413` 存、`runtime.go:286` 恢复）。

### 5.2 prompt 三段式与分开预算

当前实现把 basePrompt 与工具输出一起交给 `boundPrompt`（`runtime.go:354`），超限时保头 1/3、保尾 2/3、**丢中段**（`runtime.go:751-765`）。后果是：任务跑长后，早先加载的内容恰好漂到中段被静默丢弃，模型继续按记忆调用，直到参数出错才暴露。

改为：

```
prompt = basePrompt                                  // 不可裁（含目录）
       + renderLoaded(st.loaded)                     // 已加载区块，钉住不可裁
       + boundPrompt(renderToolEntries(st.toolCtx), 剩余预算)   // 工具输出，可裁
```

三个预算，均由 `maxPromptChars`（当前 16000，`runtime.go:110`）派生，不新增配置项：

| 量 | 值 | 含义 |
|---|---|---|
| 已加载区块上限 | `maxPromptChars / 3` | 超过则驱逐 |
| 工具输出预算 | `maxPromptChars - len(basePrompt) - len(已加载区块)` | 交给 `boundPrompt` |
| 工具输出预算下限 | `maxPromptChars / 4` | 低于此值先驱逐已加载区块，而非把工具输出压没 |

已加载区块自身超上限时按 LRU 驱逐，且**必须在区块内写明驱逐了哪些**，例如：

```
[unloaded to free space: moa_consult, fetch_url — call load_capabilities again if needed]
```

模型看得见就能自行补加载。这与中段静默丢弃是本质区别（CLAUDE.md 第 0 节）。

### 5.3 不把已加载工具提升为原生 `tools`

理由是 provider 的 prompt 缓存键包含 `tools`。evolver 的 `src/proxy/router/cache_passthrough.js:3-7` 注释明确记录了这一点：缓存键 = (model + messages + system + tools)，因此该代理刻意只重写 model 字段而不碰 `tools`。

中途向 `tools` 数组追加条目 = 整个前缀缓存作废。省下几百字节 schema，换掉整个 system + 历史的缓存命中，净亏。hermes 有 72 个工具并启用了 prompt caching，同样没有做原生提升 —— 它的 `tool_call` 永远是桥接调用。

**代价与补偿**：长尾工具的参数经 `arguments_json` 字符串传递，失去 provider 侧 schema 校验。但核心工具常驻原生，高频路径本就有原生校验；且桥接调用转发进 `tools.Execute` 时会经过 `validateInputSchema`（`internal/tool/descriptor.go:31`），校验并未丢失，只是从 provider 侧移到本地。

### 5.4 子 runtime

`newSubRuntime`（`internal/runtime/delegation.go:96` 附近）用结构体字面量构造子 runtime，绕过 `NewRuntime` 的全部默认值处理。新增 `loaded` 字段**必须同时修改它**，否则子 runtime 拿到零值。子 agent 应获得**空**的已加载集（独立上下文），此为刻意设计，需在注释中写明。

## 6. 缓存

### 6.1 磁盘侧

`SkillProvider` 在进程内缓存目录，按 mtime + size 校验失效。hermes 做了两层（进程内 LRU + 磁盘快照），本设计只做一层 —— 技能数有硬上限（§7.1），单层足够。

### 6.2 重发侧

目录每轮重新发送是协议决定的，本地缓存无法减少。唯一解法是 provider 侧 prompt 缓存，而该能力仓库已具备：

| 件 | 位置 |
|---|---|
| `InferenceRequest.StablePrefixLen` | `internal/port/ports.go:23` |
| `cache_control: ephemeral` 下发 | `internal/adapter/http_maas.go:217` |
| DeepSeek `prompt_cache_hit_tokens` 统计 | `internal/adapter/http_maas.go:257` |
| `prompt_cache` 配置开关 | `internal/config/config.go:69` |
| `basePrompt` 即稳定前缀 | `internal/runtime/runtime.go:309` |

因此**目录必须放进 `basePrompt`**（缓存断点以内）。由此产生三条硬性要求：

1. **排序确定** —— 字母序，不得依赖 map 迭代序；
2. **目录内不得含任何每轮变化的内容** —— 无计数、无时间戳、无任务 ID；
3. **已加载区块必须在断点之外** —— 它每次 `load` 都变，放进前缀等于每次加载废掉一次缓存。

**附带收益**：当前 `skillBlock` 注入的是按 `task.Input` 匹配的 top 3，意味着每个任务的 `basePrompt` 都不同，跨任务缓存必然 miss。改为全量目录后，同一 agent 的目录跨任务完全一致，前缀更长且更稳，缓存命中率上升。本改动在重发成本上不是打平，而是净赚。

## 7. 目录格式与条目契约

### 7.1 格式

置于 `basePrompt` 内：

```
<available_capabilities>
skills:
  - go-testing: 写 Go 表驱动测试、子测试与 cmp.Diff 断言
  - go-concurrency: goroutine/channel/mutex 与数据竞争排查
</available_capabilities>
Call load_capabilities with the names you need before using them.
```

工具进入目录后（阈值触发时）追加分组：

```
files:
  - read_file: Read a UTF-8 text file inside the workspace root
mcp:github:
  - github_create_issue: ...
```

块的脚手架用英文，与现有工具描述及 `lazyToolHint`（`runtime.go:830`）一致；条目内容原样取自来源。

### 7.2 条目契约（四条，均 fail-loud）

| 规则 | 违反时 |
|---|---|
| `Summary` 必填 | `Entries` 返回 error，点名具体技能文件 / 工具 |
| `Summary` ≤ 120 字符 | 返回 error，**不截断** |
| 工具必须标注 `Group` | 注册时报错 |
| 单个 agent 的技能数 ≤ **64** | `Entries` 返回 error，点名实际数量、上限、目录路径 |

`Summary` 超长选择报错而非截断：截断属于「出错了却假装没事」，而这是文件/代码作者可控的输入，报错能立即修正。技能上限 64 同理 —— 契约声明了上限，超限即违约，静默截断会让部分技能不可发现（claw-code 的坑）。

需为 `tool.Descriptor`（`internal/tool/descriptor.go:9`）新增 `Group` 字段，并为 15 个内置工具手工标注。

**技能分组**：`skill.Skill`（`internal/skill/system.go:87`）只有 `Tags []string`，无 category 字段。本期将技能统一归入单个 `skills` 组 —— 上限 64 且目录全量可见，细分无收益。`Entry.Group` 字段已就位，将来按 tag 分组只需改 `SkillProvider`。

## 8. 元工具

三个，按条件注册：

| 元工具 | 注册条件 |
|---|---|
| `search_capabilities(query, limit)` | 目录非空 |
| `load_capabilities(names)` | 目录非空 |
| `call_tool(tool_name, arguments_json)` | 目录非空 |

`call_tool` 的描述必须写明：**仅用于经 `load_capabilities` 加载的能力；原生工具直接调用**。否则模型会在「`read_file` 该直接调还是走 `call_tool`」上摇摆。

### 8.1 `load_capabilities` 契约

```
入参: names: string[]   // 必填、非空
返回: "loaded: a, b"    // 正文进入已加载区块
```

四条失败语义，全部**以失败的 ToolResult 返回给模型**（不中止任务），与现有 `call_tool` 的处理一致（`internal/runtime/lazytools.go:193`）：

| 情况 | 返回 |
|---|---|
| `names` 为空 | `load_capabilities requires at least one name` |
| 名字不在**当前 effective 目录**内 | `unknown capability "x"` |
| 单次批量 > 5 | `load at most 5 capabilities per call` |
| 已加载 | 幂等，直接返回确认 |

批量上限 5：单个技能正文可能数 KB，一次 5 个已不小，模型可连续调用。

### 8.2 `search_capabilities` 匹配

不使用 BM25。目录规模有硬上限（技能 ≤ 64），BM25 属过度设计。打分顺序：名字精确 > 名字子串 > 分组匹配 > 描述子串，确定序输出，`limit` 默认 10。

### 8.3 `call_tool` 维持现状

名字、入参、转发路径、事件发布全部不变。现有实现已经是对的：经 `tools.Execute` 因而不绕过权限/审计/超时/gate，失败作为 ToolResult 回给模型而非中止任务。仅**注册条件**从「`lazy_tools` 开关」改为「目录非空」。

## 9. 阈值门控

只作用于**工具**。技能无条件走「目录 + 按需」—— 技能没有原生形态，不存在延迟与否的选择题。

- 判据：可延迟工具的 schema 序列化字节数 ≥ 上下文窗口的 **10%**（沿用 hermes 的阈值）。
- 上下文窗口未知时退化为固定门槛 **80 000 字节**（对应 hermes 的 20K token 门槛，按 4 字符/token 粗算）。窗口大小当前不在 `port.InferenceRequest` 中，实施时若无法获得则一律走固定门槛。
- **写死策略，不是用户可配项。**
- **每任务裁决一次**，结果记入 run state 与 checkpoint，任务运行期间不翻转。

最后一条是对 hermes 的**主动偏离**（hermes 每次重算）。理由：中途翻转会让工具在模型眼前凭空出现或消失，且 `tools` 数组变化会打掉 prompt 缓存。代价是任务运行期间新接入的 MCP server 需等下一个任务才生效，本设计接受该代价。

**现状推论**：按当前 15 个工具、10 KB 的规模，阈值不会触发，工具全部保持原生下发。本期在工具侧交付的是「机制就位」，收益落在技能侧。

## 10. 作用域安全

目录、`load_capabilities`、`call_tool` **三者必须使用同一个 effective registry**（调用方传入的 `tools` 参数），**绝不可直接读 `r.tools`**。

否则 Plan 模式的只读限制会被绕过：`effectiveTools`（`internal/runtime/runtime.go:165`）已过滤掉 sensitive 工具，但若目录读取全量 registry，模型即可 `load` 到 `write_file` 的 schema 再经 `call_tool` 调用它。

现有 `dispatchToolCall`（`lazytools.go:160`）已接收 `tools` 参数，方向正确。本设计将其提升为**必须有测试钉住的不变量**（对应 hermes 的 `test_tool_call_rejects_out_of_scope_tool`）。

## 11. 迁移

### 11.1 删除 `lazy_tools` 配置项

`internal/config/config.go:201` 使用裸 `json.Unmarshal`，**没有 `DisallowUnknownFields`**。直接删除字段会导致现存配置中的 `"lazy_tools": false` 被静默忽略、行为悄然翻转 —— 正是 PR #34 在会话接口上修掉的那类兜底。

采用**墓碑字段**：`LazyTools *bool` 保留于结构体，加载时若非 nil 则返回错误，说明该选项已移除、工具懒加载现由阈值自动决定。

不选「给整个 config 加 `DisallowUnknownFields`」：该改动会波及所有历史配置中的任何多余字段，爆炸半径超出本设计范围，应独立立项。

### 11.2 技能注入切换与 `usage.Touch` 迁移

`cognitive/core.go:238` 的 `SelectForTask(task, 3)` 从 prompt 构建路径上摘除，但**不可直接删除该函数**：它在选中技能时调用 `usage.Touch`，而 Curator 依赖这份使用记录做老化清理。

停掉 `Touch` 的真实后果是 **Curator 停摆**（而非误删技能）：`internal/skill/curator.go:153-154` 写明「无使用记录的技能不会被动」，因此没有 Touch 就没有任何技能会被老化，且无人察觉。

**将 `Touch` 迁至 `load_capabilities`** —— 技能正文被真正加载时才计为「使用过」。该语义比原来更准确：原实现是关键词匹配上即计数，哪怕模型完全没有理会。

`SelectForTask` 保留（供 `/skills` 类查询与将来 `search` 的打分复用），并在其 doc 注释中写明不再参与 prompt 注入，避免后续维护者误判。

### 11.3 Checkpoint

新增 `Loaded`（已加载能力）与 `LazyDecision`（本任务阈值裁决）两项，schema 版本 +1。

旧 checkpoint 恢复时：`Loaded` 为空（合法，模型可重新加载）；`LazyDecision` **缺失时重新计算一次并写回**，不得默认 false —— 默认 false 会让恢复后的任务拿到与挂起前不同的工具面。

## 12. 验收标准

每条均需测试覆盖，并按仓库惯例做变异测试（将生产代码改回旧写法，确认测试确实失败）。

| # | 断言 | 防范 |
|---|---|---|
| 1 | 阈值触发后 `inferenceTools` 返回的数组体积确实变小 | claw-code：有壳无实 |
| 2 | Plan 模式下 sensitive 工具不出现在目录、`load` 拒绝、`call` 拒绝 | hermes：桥接成为权限绕过通道 |
| 3 | 同一 agent 连续两次构建 `basePrompt` 逐字节相同 | 目录不稳定导致缓存永远 miss |
| 4 | prompt 超限时被裁的是工具输出，已加载区块不被丢弃 | 已加载 schema 无声消失 |
| 5 | 区块驱逐时驱逐清单出现在 prompt 中 | 静默丢弃 |
| 6 | 65 个技能 → 报错；`Summary` 缺失或超 120 → 报错 | 契约不落地 |
| 7 | 配置含 `lazy_tools` → 启动报错 | 静默忽略配置 |
| 8 | checkpoint 存取 round-trip；旧版本 checkpoint 可恢复 | 迁移 |
| 9 | `load_capabilities` 之后 `usage.Touch` 确实被调用 | Curator 停摆 |

门禁沿用仓库标准：`gofmt -l .` 为空、`go build ./... && go vet ./... && go test ./...` 全绿，受影响包 `-race` 通过。

## 13. 交付切分

分两部分，各出一份实施计划。

**第一部分：技能侧闭环**（计划：`docs/superpowers/plans/2026-07-21-capability-catalog.md`）

`capability` 包与两个 provider、`Descriptor.Group`、条目契约、目录渲染与进 `basePrompt`、prompt 三段式与已加载区块、`load_capabilities`、`usage.Touch` 迁移、checkpoint 迁移、作用域安全测试。

**第二部分：工具侧门控**

阈值门控、`search_capabilities`、`call_tool` 按条件注册、删除 `lazy_tools`（墓碑字段）。

**切分依据**：必须以「能跑的闭环」为界，不能以「代码层次」为界。曾考虑把「目录进 prompt」与「`load_capabilities`」拆成两个 PR，那是错的 —— 中间态是模型看得见技能清单却没有任何办法拉取正文，比现状更糟。因此目录的接入与加载手段必须同一个 PR 落地。

第一部分完成后技能侧即可工作；第二部分只影响工具侧，且现状规模下阈值不触发。

## 14. 前提与风险

本设计依赖以下仓库现状。实施前应逐条核对是否漂移：

| 前提 | 出处 |
|---|---|
| 单条工具结果截断 4000 字符 | `internal/runtime/runtime.go:109` |
| 整体 prompt 上限 16000 字符 | `internal/runtime/runtime.go:110` |
| 超限时保头 1/3、保尾 2/3、丢中段 | `internal/runtime/runtime.go:751-765` |
| `basePrompt` 即 prompt 缓存稳定前缀 | `internal/runtime/runtime.go:309` |
| `toolCtx` 已随 checkpoint 持久化 | `internal/runtime/runtime.go:413`、`:286` |
| Curator 依赖 `usage.Touch`，无记录则不动 | `internal/skill/curator.go:153-154` |
| config 用裸 `json.Unmarshal`，不拒绝未知字段 | `internal/config/config.go:201` |
| Manual gate 遍历 registry 的 `Sensitive` | `internal/manualgate/manualgate.go:80` |
| `newSubRuntime` 用结构体字面量绕过默认值 | `internal/runtime/delegation.go:96` |
| `LazyTools` 默认已为 true | `internal/config/config.go:234` |

主要风险：

1. **模型不使用 `load_capabilities`**。若模型忽略目录直接作答，技能等于失效。缓解：目录块附明确指令；claw-code 的教训表明「只给加载工具、不给目录」会导致不可发现，本设计已通过目录规避，但模型行为仍需真实验证。
2. **技能正文体积不可控**。若单个能力的正文本身超过已加载区块上限（`maxPromptChars / 3`，当前约 5333 字符），驱逐再多也放不下。此时 `load_capabilities` 必须返回显式失败（`capability "x" is too large to load: N chars, limit M`），而不是加载一个被截断的正文 —— 截断的 schema 是非法 JSON，截断的技能正文是残缺指令，两者都比明确失败更糟。
3. **本期工具侧无可观测收益**。阈值不触发，工具全部原生下发。验收第 1 条需构造超阈值场景来验证，不能依赖真实工具集。
