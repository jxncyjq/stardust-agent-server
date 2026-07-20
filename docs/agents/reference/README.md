# Sub-Agent 配置手册

面向在 `agent.json` 中注册并使用 sub-agent（`researcher` / `writer` 等）的配置者。

本手册的每一条都对照 `legionAgent` 当前代码校验过，并在正文中标注了对应的代码位置。**配置文件里出现、但当前版本实际不生效的字段，会被显式标注**——这类字段是最容易踩的坑。

## 目录

| 文档 | 内容 |
|---|---|
| [agent-config-reference.md](./agent-config-reference.md) | `agent.json` 与 agent 配置文件逐字段参考，含生效性标注 |
| [directory-layout.md](./directory-layout.md) | 哪些目录必须存在、哪些可选、哪些建了也没用 |
| [troubleshooting.md](./troubleshooting.md) | 任务失败时如何定位真因 |
| [testing.md](./testing.md) | 门禁命令、race detector 的两种跑法、变异测试惯例 |

## 最小可用配置

`agent.json`（服务端工作目录下）注册 sub-agent：

```json
{
  "agents": {
    "researcher": "configs/agents/researcher.example.json"
  },
  "maas": {
    "default_profile": "dev",
    "profiles": {
      "dev":    { "model": "...", "base_url": "...", "api_key": "..." },
      "review": { "model": "...", "base_url": "...", "api_key": "..." }
    }
  },
  "skills": { "install_root": ".stardust/skills" }
}
```

`configs/agents/researcher.example.json`：

```json
{
  "id": "researcher",
  "role": "researcher",
  "maas_profile": "review",
  "context_files": { "enabled": true, "root": "." },
  "skills": { "install_root": "skills/researcher" }
}
```

对应关系：

- `agents` 的 **key**（`"researcher"`）是客户端提交任务时使用的 `agent_id`，也是 `GET /v1/agents` 返回的名字；
- `agents` 的 **value** 是该 agent 配置文件的路径，相对路径以 `agent.json` 所在目录为基准；
- `maas_profile` 必须能在 `maas.profiles` 中找到。

## default agent 与 sub-agent 的关键差异

提交任务时 `agent_id` 若不在注册表中（包括约定的 `default-agent`），走**默认 runtime**；命中注册表则走 **per-agent runtime**。两者能力不同：

| | default runtime | sub-agent runtime |
|---|---|---|
| workspace 工具 | 只读沙箱 | 只读沙箱 |
| task ledger / agent messaging / web | ✅ | ✅ |
| `session_search` | ✅ | ❌ |
| `moa_consult` | ✅ | ❌ |
| `delegate_task` | ✅ | ❌ |

两者的 workspace 都是只读沙箱（`tool.NewReadOnlyWorkspaceRegistry`），差异只在上面三个编排层工具。

后三项是编排层能力，被刻意排除在 worker 之外，理由写在 `internal/runtime/agent_resolver.go` 的注释里（避免无界委派树、避免跨公司/跨 agent 的历史检索越权、避免绕过 profile 分配放大成本）。这个不对称是设计，不是遗漏——`TestResolverOmitsOrchestratorOnlyTools` 锁住了它。

## 校验方式

配置改完后：

```bash
curl -s http://127.0.0.1:<port>/v1/agents
```

返回 `{"agents":["researcher","writer"]}` 即注册成功。注意返回值**不包含** default agent——它不来自注册表。

任一 agent 配置文件缺失或 JSON 格式错误，服务会在启动时直接失败并报出具体路径（`internal/agentregistry/registry.go` 的 `Load`），不会带着半套配置静默启动。
