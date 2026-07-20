# 目录要求

哪些目录需要真实存在、哪些是可选的、哪些建了也没有意义。所有路径均相对**服务端进程的工作目录**（即 `agent.json` 所在目录），另有说明的除外。

## 一、需要存在的目录

| 目录 | 来源配置 | 不存在时的后果 |
|---|---|---|
| `<用户主目录>/.stardust` | `workspace.root` 的默认值 | 启动时 WARN 一条，随后回退到同一路径 |
| `.stardust/skills` | 根 `skills.install_root` | 该 skills 根不挂载，skill 能力不可用 |
| `skills/researcher` | researcher 的 `skills.install_root` | 同上（历史版本会**导致任务失败**，见下） |
| `skills/writer` | writer 的 `skills.install_root` | 同上 |

一次建齐：

```bash
mkdir -p skills/researcher skills/writer .stardust/skills
mkdir -p ~/.stardust
```

### skills 目录缺失的行为差异（重要）

`skills.install_root` 指向的目录不存在，意味着"尚未安装任何 skill"，这是全新部署的正常状态。

- **修复后**：挂载前先做目录可用性检查（`skill.RootAvailable`），不存在则跳过挂载，任务正常执行；
- **修复前**：sub-agent 路径只判断路径字符串非空就挂载，目录扫描随即失败并终止整个任务：

  ```
  run task: build cognitive context: select task skills:
  scan skills in "skills/researcher": ... The system cannot find the path specified.
  ```

  表现为**选择任何一个 sub-agent 都必然失败**，而 default agent 正常——因为默认 runtime 一直有这个检查。

**当前状态**：修复已合入 `master`（PR #23）。用**该修复之后**构建的服务端时，skills 目录缺失只会跳过挂载并记一条 Warn 日志（`skills root unavailable, running without skills`），任务照常执行。

若运行的是更早的构建，手工创建上述目录即可绕过。

## 二、可选目录与文件

存在则生效，不存在也不会导致失败：

| 路径 | 用途 |
|---|---|
| `configs/persona/SOUL.md` | 常驻上下文，由 `context_files.soul_path` 指定 |
| `configs/persona/TOOLS.md` | 同上，`tools_path` |
| `configs/persona/USER.md` | 同上，`user_path` |
| `configs/persona/MEMORY.md` | 同上，`memory_path` |
| `~/.stardust/agents.md` | 全局 AGENTS.md |
| `<工作区>/agents.md` | 工作区 AGENTS.md |
| `<工作区>/.stardust/agents.md` | 工作区内 AGENTS.md |

AGENTS.md 的三个位置由代码推导，不可通过配置改变。

## 三、不需要建的目录

以下目录出现在 `configs/agents/*.example.json` 中，但对应配置项当前**没有任何代码消费**，建了不会产生任何效果：

- `docs/research`、`memory/researcher`（researcher 的 `workspace.docs_root` / `workspace.memory_root`）
- `docs/writing`、`memory/writer`（writer 的同名字段）

详见 [agent-config-reference.md](./agent-config-reference.md) 的"当前不生效的字段"。

## 四、运行时产生、不应手工干预

| 路径 | 说明 |
|---|---|
| `logs/agent.log` | 结构化日志，排障主要入口，已被 `.gitignore` 忽略 |
| `agent.db`、`agent.db-shm`、`agent.db-wal` | SQLite 运行时库，含任务/审计数据，不入库 |
| `agent.json` | **含真实 api_key**，`.gitignore` 已忽略，切勿提交 |

## 五、关于本手册所在目录

仓库根 `.gitignore` 包含 `docs/`，因此 `docs/` 下的全部内容（含本手册与既有的 `docs/architecture/adr-*.md`）**都不会进入 git**。

这意味着：手册只存在于本地工作树，克隆仓库的人拿不到。若需要随仓库分发，应移动到未被忽略的位置（例如 `configs/` 或仓库根的说明目录），或调整 `.gitignore` 为 `docs/` 下的特定子目录取消忽略：

```gitignore
docs/
!docs/agents/
!docs/agents/**
```
