# Agent 配置字段参考

字段语义均以代码为准，标注了对应实现位置。

## 一、`agent.json`（根配置）中与 agent 相关的字段

### `agents`

```json
"agents": { "researcher": "configs/agents/researcher.example.json" }
```

`map[名字]配置文件路径`。定义见 `internal/config/config.go` 的 `Agents map[string]string`。

- **key** 即客户端提交任务时的 `agent_id`，也是 `GET /v1/agents` 的返回项；
- **value** 为相对路径时，以 `agent.json` 所在目录为基准拼接（`agentregistry.Load`）；
- 加载失败（文件不存在 / JSON 解析失败）→ **服务启动直接失败**并报出路径，不静默跳过。

### `maas.profiles` 与 `maas.default_profile`

sub-agent 通过 `maas_profile` 引用其中一个 profile。profile 解析失败会返回
`create maas runner for profile "xxx": ...` 并使任务失败（`agent_resolver.go` 的 `ResolveTaskRunner`）。

### `skills.install_root`

根级 skills 安装根。sub-agent 未单独配置 `skills.install_root` 时回退到它（`agentSkillsRoot`）。

### `workspace.root`

会话状态与工作区的基准目录，由 `sessionstate.ResolveWorkspaceRoot` 解析：

| 配置值 | 行为 |
|---|---|
| 留空 | 使用 `<用户主目录>/.stardust`，**不告警** |
| 已配置且是目录 | 直接使用（支持 `~` 展开） |
| 已配置但不存在 / 不是目录 | **WARN** 后回退到 `<用户主目录>/.stardust` |

> 若配置值恰好等于回退值，日志会出现"fallback 到同一路径"的字样，看起来像是空转，实际含义是**该目录不存在**。建目录即可消除。

## 二、sub-agent 配置文件字段

结构定义见 `internal/agentregistry/config.go` 的 `AgentConfig`。

### 生效字段

| 字段 | 作用 | 未设置时 |
|---|---|---|
| `id` | 作为 `domain.Agent.ID` | 回退为任务的 `agent_id` |
| `role` | 作为 `domain.Agent.Role` | 空 |
| `maas_profile` | 选择 `maas.profiles` 中的推理配置 | 传空字符串给工厂 |
| `context_files.enabled` | 是否加载常驻上下文文件 | — |
| `context_files.root` | **工具沙箱根目录**，同时作为上下文文件的基准 | 回退到根配置的 `context_files.root` |
| `context_files.soul_path` | SOUL.md 路径 | 不加载 |
| `context_files.tools_path` | TOOLS.md 路径 | 不加载 |
| `context_files.user_path` | USER.md 路径 | 不加载 |
| `context_files.memory_path` | MEMORY.md 路径 | 不加载 |
| `context_files.max_file_chars` | 单文件读取上限 | — |
| `skills.install_root` | skills 扫描根目录 | 回退到根配置的 `skills.install_root` |

#### `context_files.root` 的实际优先级

工具沙箱根由 `agentToolRoot` 决定，顺序为：

1. 任务自带的 `working_dir`（会话绑定的工作目录）——最高优先；
2. 该 agent 的 `context_files.root`；
3. 根配置的 `context_files.root`。

也就是说：**一旦会话绑定了工作目录，agent 自己配置的 root 会被覆盖**。

### 当前不生效的字段

以下字段可以写、不会报错，但当前版本**没有任何代码读取它们**，配置了也不产生效果：

| 字段 | 状态 | 说明 |
|---|---|---|
| `workspace.docs_root` | **不生效** | 全代码库无消费点，仅有默认值定义 |
| `workspace.memory_root` | **不生效** | 同上 |
| `context_files.agents_path` | **已废弃** | 结构体注释标注 `deprecated: no longer used for resident loading` |
| `context_files.config_root` | **已废弃** | 结构体注释标注 `deprecated: no longer used` |

> `configs/agents/*.example.json` 里出现的 `"workspace": { "docs_root": ..., "memory_root": ... }` 属于此类。保留它们不会出错，但**不要期待 agent 的产物会写到那里**，也不需要为它们建目录。
>
> 唯一被真正使用的 workspace 字段是根配置的 `workspace.root`。

#### 关于 AGENTS.md

`context_files.agents_path` 已废弃。AGENTS.md 的加载位置由 `context_files.root` 与用户主目录推导，固定为三处（全局 `~/.stardust/agents.md`、工作区 `agents.md`、工作区 `.stardust/agents.md`），不可通过配置改变——见 `internal/config/config.go` 中 `ContextFilesConfig` 的注释。文件不存在不会导致失败。

## 三、配置变更后的生效方式

agent 注册表在**服务启动时**一次性加载（`agentregistry.Load`）。新增或修改 sub-agent 配置后需要**重启服务端**，仅重启 GUI 无效。
