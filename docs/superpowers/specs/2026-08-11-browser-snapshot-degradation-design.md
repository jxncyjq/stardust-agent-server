# 浏览器快照三级降级 + 落盘 + 翻页指针 设计

> 状态：设计待批准
> 日期：2026-08-11
> 关联：移植 hermes-agent Browser Use mode §5.1（阈值触发的三级快照降级 + 哈希去重落盘 + read_file 翻页指针）到 Legion 内置浏览器。
> 代码根：`legionAgent/internal/browser/`、`internal/tool/`、`internal/cli/`。

## 1. 背景与问题

Legion 内置浏览器（go-rod）当前把页面表示为**裁剪后的 a11y 树纯文本**，唯一压缩手段是 `BuildObservation` 按 `MaxElements`（默认 100）**按元素数截断**（`internal/browser/observation.go:42-65`）。三个问题：

1. **截断即丢**：超预算的元素直接被 `break` 丢弃，模型无法取回；`Truncated=true` 只是个布尔，没有恢复通道。
2. **阈值轴不对**：按元素数而非渲染文本体量。100 个长 `name` 元素仍可能撑爆 token；元素数与 token 成本不直接相关。
3. **生产未配**：`MaxElements` 字段在 `RuntimeConfig` 存在但 `config.BrowserConfig` 未透出（`command.go:2348-2353` 未填），恒为默认 100。

hermes-agent 的解法（`tools/browser_tool.py` §5.1）：超字符阈值时 → 有任务用辅助小模型按任务抽取 / 无则按行截断 → 全文按内容哈希去重落盘 → 提示里给 `read_file(path, offset)` 让模型翻页取回被裁内容。**省 token 又不失能**。Legion 已具备移植所需的全部基建：`read_file` 的 rune 级分页（`internal/tool/builtin.go:78 paginateRunes`，限 workspace root）、`port.MaasInferenceClient` 推理接口、`cognitive/compressor.go` 已验证的「注入 summarizer + fail-loud」范式。

## 2. 目标与非目标

**目标**
- 观测渲染文本超阈值时，按三级降级梯产出 token 可控的观测，且被裁内容可经 `read_file` 完整取回。
- 阈值改为**渲染文本 rune 数**（对齐 token 成本）；`MaxElements` 保留为次级硬上限。
- tier-1 用**辅助小模型按当前任务抽取**相关元素（方案 C，任务导向）。
- 相关配置旋钮透出到 `config.BrowserConfig`。

**非目标**
- 不引入截图/set-of-marks 视觉通道（另列 P1/P2）。
- 不新增独立「小模型」服务或模型路由；抽取器复用装配处已有的 `MaasInferenceClient` 实例（同 compressor 复用 `defaultMaas`）。将来配轻量模型时换注入实例即可，接口不变。
- 不改动 `browser_open/read/click/type/close` 的对外工具签名与 ref 语义。

## 3. 关键约束（来自现状调研）

- **抽取器无 Model 字段**：`port.InferenceRequest` 不带模型名（`internal/port/ports.go:11`），模型烘焙在 client 实例里。抽取器 = 一个 `MaasInferenceClient`，prompt 走 `InferenceRequest.Prompt` 单轮字段。
- **任务文本可达性缺口**：browser 工具 handler 只拿到 `domain.ToolCall.Arguments`（url/session_id/ref/mode），**拿不到任务文本**。任务文本是 `domain.Task.Input`（`internal/domain/types.go:75`），位于 `internal/runtime` 工具循环，需新管道下传。
- **工具根 per-agent/session 可变**：`read_file`/`write_file` 的根来自 `cfg.ContextFiles.Root`，但每 agent/session 的 `working_dir` 可能改写它（见 config-resolution-roots）。browser 是**全 daemon 单例共享**（`command.go:2337`），不知各任务的根。因此根值必须**在 `RegisterBrowserTools` 注册闭包捕获**（与 read_file 的 `absRoot` 同源同值），随 per-call 请求传入，而非存进单例 Runtime。
- **fail-loud 铁律**（`legionAgent/CLAUDE.md` §0）：禁止静默兜底；异常必报必记，错误 `%w` 包装。抽取器运行时报错**硬失败返 error**（本设计决策，见 §6）。

## 4. 架构

### 4.1 数据流

```
browser_open/read/click/type
  │  handler: 从 ctx 取 task.Input(UserTask)；闭包捕获 ToolRoot
  ▼
Req{ ..., UserTask string, ToolRoot string }
  ▼
Runtime.observe(page, sess, UserTask, ToolRoot)
  │  抽 a11y → BuildObservation（含 MaxElements 次级硬上限）→ 渲染文本
  ▼
DegradeObservation(obs, UserTask, ToolRoot, deps, cfg):
  runes(text) ≤ SnapshotRuneThreshold ?
    ├─ 是 → 原样返回（不落盘）
    └─ 否 → ① Archive.Save(root, fullText) → relPath（sha256 去重）
            ② extractor != nil 且 UserTask != "" ?
                 ├─ 是 → reduced, err := extractor.Extract(ctx, UserTask, fullText)
                 │         err != nil → 返回 error（硬失败，§6）
                 │         reduced 仍超阈 → 进 ③（对 reduced 截断）
                 └─ 否（未配抽取器 / 无任务）→ 进 ③
            ③ TruncateByLine(text) — 按行边界截断，不切碎元素
            footer: "[已裁剪 第X-Y字/共M字；全文见 read_file(path=<relPath>)]"
```

### 4.2 组件（各单一职责、经接口解耦）

| 单元 | 位置 | 职责 | 依赖 |
|---|---|---|---|
| `SnapshotExtractor` 接口 | `internal/browser`（新） | `Extract(ctx, task, full string) (string, error)` | 无（browser 不 import port/LLM，守平台无关） |
| extractor 适配实现 | `internal/cli` 或 `internal/adapter`（新） | 包 `MaasInferenceClient`，拼 prompt=任务+全文，调 `Generate` | port |
| `SnapshotArchive` 接口 | `internal/browser`（新） | `Save(root, content string) (relPath string, err error)`、`Cleanup(root string, ttl)` | 无 |
| `fileSnapshotArchive` 默认实现 | `internal/browser`（新文件 `snapshot_archive.go`） | sha256(content) 命名，落 `<root>/<ArchiveDir>/<sha>.txt`，存在即跳过（去重），返回相对 root 路径 | os |
| `DegradeObservation` | `internal/browser/observation.go`（新纯函数） | 编排阈值判定 / 落盘 / 抽取 / 截断 / footer；LLM 与 IO 全经接口注入，可表驱动测试 | 上述接口 |
| `TruncateByLine` | `observation.go`（新纯函数） | 按 `\n` 边界截到 rune 预算内，不切碎单个元素行 | 无 |
| 管道：ctx 注入任务 | `internal/runtime`（dispatch 处） | `context.WithValue` 注入 `task.Input`；browser handler 反向读取 | 无 |
| 管道：ToolRoot 捕获 | `internal/tool/browser.go` + `BrowserToolOptions` | 注册闭包捕获与 read_file 同源的根 | 无 |

### 4.3 Runtime 与装配变更

- `RuntimeConfig` 新增：`Extractor SnapshotExtractor`（可空）、`Archive SnapshotArchive`（可空→装配默认实现）、`SnapshotRuneThreshold int`、`SnapshotTTL time.Duration`、`SnapshotArchiveDir string`。
- `observe` 签名扩展为携带 `userTask, toolRoot`（`Open/Read/Click/Type` 各自把 Req 里的值透传）。`OpenReq/ReadReq/ClickReq/TypeReq` 增 `UserTask`、`ToolRoot` 字段。
- `command.go:2348` 装配：注入 `defaultMaas` 包装的 extractor + `fileSnapshotArchive` + 从 `cfg.Browser` 读阈值/TTL/dir。
- `RegisterBrowserTools`：`BrowserToolOptions` 增 `ToolRoot string`；各 handler 从 ctx 取 UserTask、用捕获的 ToolRoot，填进 Req。

## 5. 配置（透出到 `config.BrowserConfig`）

| 字段 | 默认 | 说明 |
|---|---|---|
| `snapshot_rune_threshold` | 15000 | 渲染文本超此 rune 数触发降级；`<=0` 关闭降级（保持原样，仅 MaxElements 生效） |
| `max_elements` | 100 | **顺带修复**：透出既有字段，作次级硬上限 |
| `snapshot_ttl_hours` | 24 | 落盘全文保留时长；过期清理 |
| `snapshot_archive_dir` | `.legion/browser/snapshots` | 相对工具根的落盘子目录 |

## 6. fail-loud 交互（已定案）

- **抽取器未配置（nil）**：契约声明的可选槽 → 跳过 tier-2，直接 tier-3 截断。**非兜底**（契约允许其不存在，同 compressor `Summarizer == nil`）。
- **抽取器运行时报错**：**硬失败** —— `DegradeObservation` 返回 `fmt.Errorf("browser snapshot extract: %w", err)`，`observe` 上抛，`browser_open/read/click/type` 如实失败。与 `cognitive/compressor.go:140` 一致（summarize 失败则整体失败）。理由：抽取是声明的 tier-2，配置了却失败属非预期状态，按铁律响亮失败，不静默降级掩盖模型/网络故障。
- **落盘 IO 失败**：返回 error。全文没存成却给模型翻页指针 = 骗模型，必须响亮失败。
- **抽取器成功但返回空文本**：视为异常（配置了抽取器却产出空观测），返回 error，不拿空串冒充。
- 记录：以上错误在 `browser.go` handler 边界已有 `failure(call.ID, err.Error())` 通道回给模型；同时 Runtime 侧对 IO/抽取失败用项目 logger 结构化 **Error** 记（带 url / session_id / 字节数）。

## 7. 落盘细节

- 命名：`sha256(content)` 十六进制 + `.txt`；同内容同名 → `Save` 见文件已存在即跳过写、直接返回路径（内容哈希去重，同页重复观测不重复落盘）。
- 路径：`<toolRoot>/<snapshot_archive_dir>/<sha>.txt`；返回**相对工具根**的 `relPath`，footer 里原样给 `read_file(path=relPath)`，模型在同一根下可翻页。
- 权限：文件由 Go 侧写（非 write_file 工具），模型只读；只需模型具备 `read_file`（默认具备）。**边界情形**：若某 agent 被 `disabled_tools` 禁了 `read_file`，翻页指针失效——footer 仍给出，属该 agent 配置取舍，记 Warn 不特殊处理。
- 清理：`fileSnapshotArchive` 在 Runtime 启动时清一次 + reaper 周期顺带清 `<dir>` 下 mtime 超 TTL 的文件。清理失败记 Warn 不致命（不影响主流程）。
- `.gitignore`：落盘目录应加入工具根的忽略（避免污染用户仓库）；实现阶段在装配处 or 文档提示。

## 8. 测试策略

- `DegradeObservation` 表驱动纯函数测试：阈值边界（等于/略超/远超）、截断按行不切元素、footer 字节数与路径格式、reduced 仍超阈再截断。
- fake `SnapshotExtractor`：成功（返回 reduced）、报错（断言整体返 error）、返回空串（断言返 error）、nil（断言走 tier-3 不报错）——覆盖全部 §6 分支。
- fake `SnapshotArchive`：验 sha 去重（同内容一次写）、IO 失败上抛 error。
- `fileSnapshotArchive` 真实文件测试：落盘路径正确、去重跳写、TTL 清理只删过期。
- 集成：`browser.go` handler 经 ctx 取到 UserTask、用捕获 ToolRoot 填 Req；无任务/无抽取器时优雅降级。
- 回归：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。错误路径（§6 三类失败）必须有断言「确实返回 error / 确实记录」。

## 9. 影响面与风险

- **签名扩散**：`observe` 及 4 个 `*Req` 加字段、`BrowserToolOptions` 加 `ToolRoot`、`RuntimeConfig` 加 5 字段——改动点集中在 browser + tool + cli 三处，无跨仓（纯 legionAgent）。
- **ctx value 传任务文本**：需在 runtime dispatch 唯一处注入，browser handler 唯一处读取，定义私有 ctx key 类型避免碰撞。
- **单例共享 + per-call 根**：设计已把根/任务/抽取器都做成 per-call 或注入，Runtime 不持有任务态，与「单例共享」不冲突。
- **延迟**：tier-2 抽取引入一次 LLM 往返（仅超阈时）。可接受——超阈本就是大页面，抽取换 token 与相关性划算；小页面（≤阈值）零额外开销。

## 10. 交付顺序（供 writing-plans 细化）

1. 纯函数层：`TruncateByLine` + `DegradeObservation` + 接口定义 + 表驱动测试（无 IO/LLM，先绿）。
2. `fileSnapshotArchive` 落盘实现 + TTL 清理 + 文件测试。
3. extractor 适配实现（包 MaasInferenceClient）+ 测试。
4. 管道：`observe`/`*Req` 加字段、ctx 注入任务、`BrowserToolOptions` 加 ToolRoot、handler 接线。
5. 配置透出 `config.BrowserConfig` + 装配 `command.go`/`app.go` 注入。
6. 集成测试 + 回归全绿。
