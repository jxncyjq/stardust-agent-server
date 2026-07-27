---
title: AGENTS.md 分层加载（Claude Code 式单包含链）设计
date: 2026-07-27
status: approved
scope: legionAgent — contextfiles 常驻上行链 + 工具按需下行子目录链
---

# AGENTS.md 分层加载设计

## 目标

让 agents.md 的加载对齐 Claude Code 的**单一包含链**模型：全局(home) → 项目根 → 子目录，层层嵌套、越具体越强。消除现有「serve cwd 与 working_dir 两棵不相关树同时作项目根」的矛盾。

## 背景与现状

- 现有 `internal/contextfiles/loader.go` 已有：`Load`（常驻加载 3 个固定 agents.md：`~/.stardust/agents.md`、`<root>/agents.md`、`<root>/.stardust/agents.md`）、`NearestAgentsFile`（向上走取最近一个）、`LoadAgentsFile`（可复用单文件读，沙箱+注入扫描+截断）、`ResidentAgentsPaths`（write_file 去重用）。
- 现有 `internal/tool/builtin.go` `nearestAgentsNote`：**仅 write_file** 结果后追加**最近一个**子目录 agents.md，跳过常驻路径。
- 现有 `root` = `ContextFiles.Root`（= serve 进程 cwd，见 memory `legion-config-resolution-roots`）。这是矛盾根源：serve cwd 是 legionAgent 自己的仓库，与用户 working_dir 项目无包含关系。

## 核心决策（已确认）

1. **单一项目根**：`projectRoot = task.WorkingDir（设了就用）→ else agentCfg.ContextFiles.Root → else rootConfig.ContextFiles.Root`（复用现有 `agentToolRoot` 的解析，使 agents.md 根与文件工具根统一）。serve cwd 只在无 working_dir 时作回退，**不再作平行第二根**。
2. **persona 与 agents.md 分离**：SOUL/TOOLS/USER/MEMORY 是 agent 身份层，仍锚 serve cwd/config，**不进 agents.md 优先级链**，不参与本设计。
3. **单包含链，弱→强（拼接顺序，靠后更强）**：
   ```
   ~/.stardust/agents.md                       全局基底（trusted，免沙箱）
   home→…→projectRoot 沿途祖先 <dir>/agents.md   常驻上行链（免沙箱，trusted）
   <projectRoot>/agents.md + <projectRoot>/.stardust/agents.md   常驻
   projectRoot→…→被碰文件目录 <dir>/agents.md    按需下行子目录链
   ```
4. **上行 vs 下行的常驻/按需划分**：
   - `projectRoot` 及**其上**（含 home、沿途祖先）在**任务开始常驻加载**（对整个 session 固定）。
   - `projectRoot` **之下**的子目录 agents.md **按需注入**（依赖当轮碰了哪个文件）。
5. **按需触发工具** = read_file / search_content / write_file（读+写都触发）。
6. **祖先链全收**（非最近一个）：下行链收 projectRoot→…→文件目录沿途**每层** agents.md。
7. **去重**：任务级已注入集（`map[string]bool`+mutex）+ 常驻路径集。同一 agents.md 一个任务只注入一次，防 token 重复膨胀（见 memory `legion-token-multiround-debug-probe`）。
8. **沙箱边界**：`projectRoot` 及以下 → `isWithinRoot(projectRoot, …)` 沙箱校验；`projectRoot` **之上**（祖先目录、home）→ 免沙箱 trusted 读（是用户自己的文件系统，等同现有 global slot 处理），仍走注入扫描 + 截断。

## 架构（两部分，可拆两 PR）

### 部分 A：常驻上行链（需求 1 + 上行）

**contextfiles 层**
- `Config` 用 `projectRoot` 语义（调用方传 `agentToolRoot` 结果）；保留 persona 路径字段（仍相对 serve cwd/config 解析——persona 不动）。
- 新增 `AncestorAgentsChain(projectRoot, homeDir, maxChars) ([]AgentsEntry, error)`：从 `projectRoot` **向上**走到 `homeDir`（含 home）逐目录 `findAgentsFile`，收集存在者。`projectRoot` 之上免沙箱、trusted 读（注入扫描+截断仍做）。返回顺序 home 侧→projectRoot 侧（弱→强）。
- `Block` 调整：`GlobalAgents`(~/.stardust) 保留；新增 `AncestorAgents []AgentsEntry`（上行链，projectRoot 之上）；`ProjectAgents`+`ProjectStardustAgents`（= 原 WorkspaceAgents/StardustAgents，锚 projectRoot）。`Render` 按弱→强输出。
- `AgentsEntry{RelOrAbsLabel string; Content string; Blocked bool}`（label 用于 section 标题定位）。
- `ResidentAgentsPaths` 扩展为收「全局 + 上行链每层 + projectRoot 两处」全部路径，供按需去重跳过。

**caller（agent_resolver.go）**
- `loadAgentContextFiles` 传入 `projectRoot = agentToolRoot(...)`（现在传的是 config root）+ `homeDir`。
- persona 加载路径不变。

### 部分 B：按需下行子目录链（需求 2）

**contextfiles 层**
- 新增 `SubtreeAgentsChain(projectRoot, fileDir, maxChars) ([]AgentsEntry, error)`：从 `projectRoot`（不含，已常驻）**向下**到 `fileDir` 逐层 `findAgentsFile`，沙箱校验（都在 projectRoot 内），返回 shallow→deep（弱→强）。（`NearestAgentsFile` 保留不删，供其它潜在调用；本设计不用它。）

**tool 层（builtin.go）**
- `workspaceRegistryOptions` 加任务级 `injectedAgents *injectedSet`（`struct{ mu sync.Mutex; seen map[string]bool }`），构造于每任务 registry 装配处，初始塞入 `ResidentAgentsPaths` 全集。
- 新增 `subtreeAgentsNote(projectRoot, fileDir, options)`：取 `SubtreeAgentsChain`，逐条跳过 `injectedAgents.seen` 已有者，未见者标记并渲染追加；blocked 者渲染「已忽略」提示。
- 接入 read_file / search_content / write_file 三个 handler：操作成功后，以目标路径所在目录为 `fileDir` 调 `subtreeAgentsNote`，把返回文本追加到 `ToolResult.Output`。
  - read_file/write_file：`fileDir = filepath.Dir(目标文件)`。
  - search_content：`fileDir = 搜索的 directory 参数解析后的目录`（命中所在目录粒度过细，取搜索根即可）。
- 移除 / 替换旧 `nearestAgentsNote`（write_file 专用、最近一个）为统一的 `subtreeAgentsNote`（全链+去重+三工具）。

## 数据流（一次带 working_dir 的会话）

1. 任务开始，`agent_resolver` 算 `projectRoot = task.WorkingDir`。
2. `contextfiles.Load` 常驻加载：`~/.stardust/agents.md` + 上行链(home→projectRoot) + projectRoot 两处 + persona。拼进 system context。
3. `ResidentAgentsPaths` 全集塞进任务级 `injectedAgents.seen`。
4. 模型 read_file `<projectRoot>/sub/deep/x.go` → handler 读文件成功 → `subtreeAgentsNote(projectRoot, <projectRoot>/sub/deep)` 取链：`sub/agents.md`、`sub/deep/agents.md`（projectRoot 自身已常驻，跳过），未注入者追加 + 记入 seen。
5. 后续再读 `sub/deep/y.go` → `sub/agents.md`、`sub/deep/agents.md` 已在 seen → 不重复注入（去重生效）。

## 错误处理（fail-loud，守 CLAUDE.md 铁律）

- 读文件失败（非 NotExist）、沙箱越界 → 返回 error（`fmt.Errorf` 包装），现有 `readOne` 已如此，新链函数一致传播。
- 缺文件（NotExist）→ 契约允许的可选，跳过不报错（agents.md 本就多数目录没有）。
- unsafe 注入内容 → 记 Blocked / 渲染「已忽略」提示，不喂内容。
- 上行链读到 home 之上不应发生（walk 上界 = homeDir）；若 projectRoot 不在 home 之下（异常拓扑）→ 上行 walk 在到达文件系统根或 home 任一先到即停，不 panic。
- 去重集并发：tool 调用在工具循环内多为串行，但委派/并发场景下 `injectedAgents` 加 mutex 保护，避免竞态。

## 测试

**contextfiles**
- `AncestorAgentsChain`：projectRoot 之上多层各有 agents.md → 全收 + 顺序弱→强 + 免沙箱；home 的 `.stardust/agents.md` 仍单独走 global slot；缺层跳过；unsafe 拦截；读错误 fail-loud。
- `SubtreeAgentsChain`：projectRoot→fileDir 多层全收 + shallow→deep + 沙箱内；越界报错。
- 双端退化：projectRoot == serve cwd（无 working_dir）时无重复；projectRoot == home 时上行链为空。

**tool（builtin）**
- read_file/search_content/write_file 碰子目录各触发一次注入。
- 同一 agents.md 重复碰只注入一次（去重集）。
- 常驻路径不被按需重注。
- unsafe 子目录 agents.md → 「已忽略」提示不喂内容。

**门禁**：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 空。

## 交付拆分

- **PR A**：contextfiles 常驻上行链 + projectRoot 统一 + Block/Render/ResidentAgentsPaths 扩展 + agent_resolver 接线 + 单测。
- **PR B**：`SubtreeAgentsChain` + 任务级去重集 + read/search/write 三工具接入 + 替换旧 nearestAgentsNote + 单测。**依赖 PR A**（共享 projectRoot 定义与 ResidentAgentsPaths 扩展）。

## 非目标

- persona（SOUL/TOOLS/USER/MEMORY）加载逻辑与根（不动，仍 serve cwd/config）。
- 语义检索 / embedding（另事）。
- agents.md 之外的上下文文件分层。
- 跨 session 缓存 agents.md（每任务按 projectRoot 重算）。

## 相关

memory：[[legion-config-resolution-roots]]（三根语义、projectRoot 来源）、[[legion-token-multiround-debug-probe]]（去重防 token 膨胀）。
