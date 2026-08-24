# A5c 插件安装与授权语义 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让「装一个插件」和「授权一个插件」变成两件分开的、都留痕的事——`legion plugin install` 只负责把一个验证过的包登记进目标态且**默认零授权**，授权是之后一次显式动作。

**Architecture:** `install` 复用 A5b 的取回/校验/缓存与 A5a 的验签，拿到可信包后读它的 `plugin.json`，向 `plugins.json` 追加一条 **`enabled: false`、`grant.capabilities` 为空**的条目。「装了 / 启用了 / 授权了」是三件事，配置里各有其位。授权走 `legion plugin grant`，它是同一套语义的第一个消费者；GUI 同意流是下一期的第二个消费者，本期只把语义与接口做实并测透。

**Tech Stack:** Go 1.26.0，module `github.com/stardust/legion-agent`。**不新增任何第三方依赖**。

**不含**（各自独立 plan）：GUI 授权同意流（下一期，本期交付它要消费的后端语义）、`legion plugin search` 与任何索引/市场概念、OCI registry 传输、密钥吊销与透明日志、缓存清理与容量上限。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：禁止 fallback、禁止静默吞错、禁止零值假装正常；错误用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装；不变量违反用 `panic`。
- 公开 API 必须有 Go doc 风格注释，以标识符名开头，且不得与代码矛盾。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿，`gofmt -l .` 输出为空。
- 涉及并发的任务额外跑 `go test -race`，**plugin / runtime / cli 包串行跑**（`-p 1`）。
- 错误路径必须有测试断言「确实返回 error」，不得只测 happy path。
- 每个 task 完成后做一次**变异验证**：把该 task 的核心机制改坏，确认测试确实 FAIL，把失败输出留在报告里，然后还原。
- 所有测试用 `httptest.Server`，**不得触真实网络**。
- 设计依据：docs 仓 `docs/design/architecture/legion-plugin-system.md` §7（安全模型）、§9 路线图 P3-A5c。

## 四条已定的裁决（实施时不再讨论）

1. **`install` 默认零授权。** 写入的条目 `grant.capabilities` 为空，要用得显式 `--grant`，或事后 `legion plugin grant`。这与「声明 ∩ 授权」同源：**安装不等于授权**，插件想要什么从来不是安全边界。
2. **本期不做 `search`。** 仓内没有 registry 概念，设计文档 §10 明写「不做插件市场」——没有索引源的 search 要么是空壳，要么就是在造市场。Task 5 把路线图那行同步改掉。
3. **`TOOLS.md` 不是授权源。** 它是注入给模型的上下文文件（`config.ContextFiles.ToolsPath`，`internal/cli/command.go:360`），内容是自然语言策略。真正的授权仍然只在 `plugins.json`，由实例化时绑定哪些 host function 决定。
4. **插件工具不写进 `TOOLS.md`。** 它们本来就经 `tool.Registry` 进能力目录，模型已经看得到；再写一份等于重复，而且 `TOOLS.md` 进每个任务的 prompt——往里加条目就是在动 prompt 前缀，正是契约 4 存在的理由（实测：目录中段插一条，DeepSeek 首发命中归零、Kimi 从 1792 掉到 768）。

## ⚠️ 「装了 / 启用了 / 授权了」为什么必须是三件事

A5a 的 `AssembleSpec` 有一条硬规则：`entry.Tools` 为空即拒绝（`internal/plugin/manifest/assemble.go:311`），且能力**声明 ⊄ 授权**也是拒绝加载。于是一条「装上但没授权」的条目如果写成 `enabled: true`，会立刻在 `plugins status` 里变成一条红的 `failed`——运维分不清「装坏了」和「还没授权」，而一行永远红着的状态就是训练人忽略状态的开始（A5a 的验收恰好顶出过同形状的缺陷）。

因此 `install` 写入的条目是：

```json
{
  "name": "legion-jira",
  "source": "https://pkgs.example.com/legion-jira-1.2.0.tar.gz",
  "digest": "sha256:…",
  "enabled": false,
  "tools": [ { "name": "jira_search" } ],
  "grant": { "capabilities": [] }
}
```

`tools` 列全是**记录插件提供什么**，不是接受；`enabled: false` + 空 `capabilities` 才是「尚未授权」的表达。`grant` 把它变成 `enabled: true` 并填上能力。

## ⚠️ Fork-bomb 安全规程（本仓已因此宕机两次）

`host.test.exe` 曾吃到 **170 GB 虚存**（Windows 事件 2004 + Kernel-Power 41），原因是测试把被测功能当唯一终止条件。

1. 任何循环边界必须独立于被测功能：轮数写成字面量（≤5），每轮断言实例数上限。
2. 每次 `go test` 带 `-timeout 120s`；plugin / runtime / cli 包用 `-p 1`。
3. 跑挂或内存上涨立刻杀掉、先加边界再重试。
4. 绝不把变异留在工作区，每次还原后用 `git status` 核对。

## 前置事实（已在 master `4d5ce31`，直接用）

```go
// internal/plugin/manifest
type Entry struct {
    Name, Source, Digest string
    Enabled              bool          // 缺省 true；install 显式写 false
    Grant                GrantDecl     // Capabilities / AllowedHosts / AllowedPaths
    Tools                []ToolAccept  // 非空，否则 AssembleSpec 拒绝
    Config               json.RawMessage
}
type Deployment struct{ Plugins []Entry }
func ParseDeployment(data []byte) (Deployment, error)   // 拒未知字段、拒尾部残留、拒重名
func ParsePlugin(data []byte) (PluginManifest, error)
func LoadPackage(dir string, keyring *sign.Keyring) (PluginManifest, []byte, error)
func (e Entry) IsRemote() bool
func (e Entry) IsInsecureSource() bool

// internal/plugin/fetch
func Fetch(ctx context.Context, client *http.Client, u *url.URL, digest string, limits Limits) ([]byte, error)
func NewCache(root string) (*Cache, error)
func (c *Cache) Has(digest string) (bool, error)
func (c *Cache) Put(digest string, archive []byte, limits UnpackLimits) (string, error)

// internal/plugin/sign
func ParseKeyring(data []byte) (*Keyring, error)

// internal/cli —— 既有 plugins 子命令：status / reload / keygen / sign / pubkey
```

---

### Task 1: 条目草案的合成

**Files:**
- Create: `internal/plugin/manifest/draft.go`
- Test: `internal/plugin/manifest/draft_test.go`

**Interfaces:**
- Produces:
  - `func DraftEntry(pm PluginManifest, source, digest string) (Entry, error)`

**纯函数**：拿一份**已经验证过**的 `PluginManifest` 加上来源与摘要，产出一条可以直接写进 `plugins.json` 的条目草案。不碰文件系统、不发请求、不读配置——验证是调用方的事，写盘是 Task 2 的事。

**五条规则，逐条要有测试：**

1. `Enabled` 恒为 `false`。**不提供参数让调用方改它**：一条「装完即启用」的路径一旦存在，就一定会有人在脚本里用它。
2. `Grant.Capabilities` 恒为空切片。同理，不提供在这一层直接授权的入口。
3. `Tools` 由 `pm.Tools` 逐条映射出 `ToolAccept{Name: …}`，顺序与插件清单一致，**不带任何 RiskLevel/Sensitive 覆盖**——覆盖是部署方收紧用的，草案不替人做决定。
4. `Name` 取自 `pm.Name`（不是调用方传的），因为条目名必须与插件自称一致，A5a 的身份检查会拒绝不一致的。
5. `pm.Tools` 为空 → 报错。这样的插件装进去也会被 `AssembleSpec` 拒，早报比晚报好，且错误能点名插件。

- [ ] **Step 1: 写失败测试**（上述五条 + 正向：逐字段断言草案内容，含 `Tools` 顺序）

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证**：让 `Enabled` 跟随一个参数（默认 true）→ 规则 1 的测试应 FAIL。

- [ ] **Step 4: 提交**

```bash
git commit -m "feat(plugin): draft a deployment entry from a verified plugin manifest"
```

---

### Task 2: `plugins.json` 的安全改写

**Files:**
- Create: `internal/plugin/manifest/edit.go`
- Test: `internal/plugin/manifest/edit_test.go`

**Interfaces:**
- Produces:
  - `func AddEntry(dep Deployment, entry Entry) (Deployment, error)`
  - `func UpdateEntry(dep Deployment, name string, mutate func(Entry) (Entry, error)) (Deployment, error)`
  - `func MarshalDeployment(dep Deployment) ([]byte, error)`
  - `func WriteDeployment(path string, dep Deployment) error`

**六条规则，逐条要有测试：**

1. `AddEntry` 遇到同名条目 → 报错点名该名字。**不静默覆盖**：覆盖一条既有条目会把别人已经做过的授权决定悄悄抹掉。
2. `UpdateEntry` 找不到名字 → 报错点名该名字与现有条目名列表。
3. 新条目**追加在末尾**，既有条目顺序一字不动。顺序是人读这份文件的锚点。
4. `MarshalDeployment` 产出的字节必须能被 `ParseDeployment` 原样读回（往返一致），且缩进风格与仓内既有 JSON 配置一致。
5. `WriteDeployment` **原子落盘**：写临时文件再 rename。一次中断的写入不能留下半份目标态——那会让下次启动读到一份被截断的配置。
6. 写之前先 `ParseDeployment` 自校验一遍产出的字节：**绝不写出一份自己都读不回来的配置**。

- [ ] **Step 1: 写失败测试**（上述六条；往返用例要断言具体字段而非只比字节）

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（两个）**
  - 让 `AddEntry` 同名时覆盖 → 规则 1 的测试应 FAIL
  - 把原子写换成直接写目标文件 → 规则 5 的测试应 FAIL

- [ ] **Step 4: 提交**

---

### Task 3: `legion plugin install`

**Files:**
- Modify: `internal/cli/plugins_command.go`（新增子命令）
- Test: `internal/cli/plugins_command_test.go`

**命令形状**：`agent plugins install <url> --digest sha256:<hex> [--grant log,http] [--config <file>]`

**顺序不可颠倒**（写进命令的 doc）：

1. `Fetch`（摘要把守，不符不落盘）
2. `Cache.Put`（解包 + 原子就位）
3. **`LoadPackage`（验签）** —— 没过这一关**绝不**写配置
4. `DraftEntry` → `AddEntry` → `WriteDeployment`

**九条规则，逐条要有测试：**

1. **验签失败 → 不写配置**，`plugins.json` 一个字节都不变。这是本 task 的核心不变量。
2. 摘要不符 → 不写配置，缓存目录里不留东西。
3. `--digest` 缺失 → 报错。远程条目的摘要是必填的，命令层不该给人「先装了再说」的口子。
4. `--grant` 未给 → 写入的条目 `capabilities` 为空且 `enabled: false`；命令输出明确告诉运维「已登记但未授权，用 `plugins grant` 授权后 reload 生效」。
5. `--grant` 给了插件没声明的能力 → 报错点名该能力。授权一个插件没要的能力是配置错误，不是宽容。
6. 同名插件已在 `plugins.json` → 报错（Task 2 规则 1 的传递），并提示用 `plugins grant` 或手工编辑。
7. `plugins.manifest` 未配置 → 报错说明 install 需要一份目标态清单。
8. 明文 `http://` 而 `allow_insecure_sources` 未开 → **取回之前就拒绝**。`remoteDir` 在 loader.go:1335 已有这条；install 不做就成了绕过它的洞——运维能经明文装进来，直到 serve 才发现部署根本不肯拉。
9. `plugins.cache` 未配置 → 报错点名该设置。远程包总得写到某处，而写哪儿是部署决定（措辞与 loader.go:1330 保持一致）。

**命令不重启也不 reload**：改的是磁盘上的目标态，生效要 `agent plugins reload`（且 A5b 已有「远程策略漂移就拒绝 reload」的保护）。输出里说清楚这一点。

- [ ] **Step 1: 写失败测试**（上述七条，全部 `httptest.Server`）

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（两个）**
  - 把验签挪到写配置之后 → 规则 1 的测试应 FAIL
  - `--grant` 未给时默认按插件声明全授 → 规则 4 的测试应 FAIL

- [ ] **Step 4: 提交**

---

### Task 4: 授权语义与 `legion plugin grant|deny`

**Files:**
- Modify: `internal/cli/plugins_command.go`
- Modify: `internal/plugin/loader/loader.go`（`InstanceStatus` 增「未授权」呈现所需字段）
- Test: `internal/cli/plugins_command_test.go`、`internal/plugin/loader/status_test.go`

**Interfaces:**
- Produces：`agent plugins grant <name> --capabilities log,http [--allowed-hosts …] [--allowed-paths …]`、`agent plugins deny <name>`

**语义**：

- `grant` 把该条目的 `grant` 段填上并置 `enabled: true`；**只接受插件声明过的能力**，多给即报错（与 install 的规则 5 同源）。
- `deny` 把条目置回 `enabled: false` 且清空 `capabilities`——**回到「装了但未授权」而不是删除条目**，因为删除会连带丢掉 source 与 digest 这两条人工填回很麻烦的信息。
- 两者都经 Task 2 的 `UpdateEntry` + `WriteDeployment`，因此同样原子、同样自校验。
- 两者都**不触碰运行中的进程**，输出提示要 reload 才生效。

**`plugins status` 要能看出「未授权」**：一条 `enabled: false` 且 `capabilities` 为空的条目，状态显示为 `unauthorized`（而不是与「运维手工停用」的 `disabled` 混为一谈），并提示用 `plugins grant`。两者的区别是：`disabled` 是「我不想要它跑」，`unauthorized` 是「它还没被允许」——运维要做的下一步完全不同。

**四条规则，逐条要有测试：**

1. `grant` 后条目 `enabled: true` 且能力齐全，reload 之后插件真的挂载（端到端在 Task 5）。
2. `grant` 给未声明的能力 → 报错点名。
3. `deny` 后条目回到 `enabled:false` + 空能力，且 `source`/`digest`/`tools` 一字未动。
4. `status` 能把 `unauthorized` 与 `disabled` 分开显示，各自带可操作的下一步。

- [ ] **Step 1: 写失败测试**（上述四条）

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（两个）**
  - `grant` 不校验能力是否被声明 → 规则 2 的测试应 FAIL
  - `status` 把 `unauthorized` 并进 `disabled` → 规则 4 的测试应 FAIL

- [ ] **Step 4: 提交**

---

### Task 5: 端到端验收与文档回写

**Files:**
- Test: `internal/plugin/loader/e2e_test.go`（在既有验收文件里追加）
- Modify: docs 仓 `docs/design/architecture/legion-plugin-system.md`（§7 补「安装 ≠ 授权」；§9 路线图 A5c 标交付并去掉 `search`）

**⚠️ 本 task 反复挂载真 wasm 实例，两套规程逐条适用**：轮数写死 ≤5，每轮断言实例数上限，全部用 `httptest.Server`，`-timeout 120s`，`-p 1`。

- [ ] **Step 1: 安装到授权的完整闭环**

```text
keygen 产钥 → sign 给包签名 → 打 tar.gz → httptest.Server 提供
  → agent plugins install <url> --digest … → plugins.json 多出一条 enabled:false、capabilities 空的条目
  → reload → 该插件**没有**挂载，status 显示 unauthorized 并给出下一步
  → agent plugins grant <name> --capabilities log → reload → 插件挂载、工具进注册表
  → agent plugins deny <name> → reload → 工具消失，条目仍在且 source/digest 未丢
```

- [ ] **Step 2: 拒绝路径**

- 签名不过 → `plugins.json` 一字节未变（**装不进去，而不是装进去再失败**）
- `--grant` 给插件没声明的能力 → 报错点名，配置未变
- 同名插件已存在 → 报错，既有条目的授权原样保留

- [ ] **Step 3: 全量回归**

```bash
go build ./... && go vet ./... && go test ./... -count=1 -timeout 300s && gofmt -l .
```

再逐包串行跑 `-race`（至少 `internal/plugin/...`、`internal/cli`）。

- [ ] **Step 4: 文档回写**

§7 补一句：安装只登记来源与摘要，**授权是独立的一次显式动作**，`plugins.json` 里 `enabled:false` + 空 `capabilities` 表示「已登记未授权」。§9 路线图 A5c 标交付，**并把 `search` 从该行去掉**——仓内没有索引源，而市场是 §10 明确排除的；GUI 同意流标为下一期，说明它消费的就是本期这套语义。

- [ ] **Step 5: 提交并开 PR**

---

## 交付后状态

- `agent plugins install <url> --digest …` 一条命令完成取回、验摘要、验签、登记
- 登记的条目**默认零授权**：`enabled:false` + 空 `capabilities`，装了不等于能跑
- `agent plugins grant|deny` 是授权的显式动作，只接受插件声明过的能力，`deny` 保留来源与摘要
- `plugins status` 把「未授权」与「手工停用」分开，各自给出可操作的下一步
- GUI 同意流下一期直接消费这套语义，不必再造一遍

**尚未包含**：GUI 授权同意流、`search` 与任何索引/市场、OCI registry 传输、密钥吊销与透明日志、缓存清理与容量上限。
