# Legion Agent Runtime Docs

这个目录是 Legion Agent 运行时书写经验文档、任务总结、操作说明和复盘材料的默认目标目录。

它不同于工作区根目录的 `docs/agents/legion-agent/`：

- `docs/agents/legion-agent/`：开发者维护的项目设计、计划、接口和运维文档。
- `legion/legionAgent/docs/`：Agent 运行时产出的经验文档和工作成果文档。

## 建议内容

- `runbooks/`：Agent 在执行任务后沉淀的操作手册。
- `lessons/`：任务复盘、踩坑记录、经验总结。
- `reports/`：面向用户或系统的阶段性报告。
- `decisions/`：Agent 在任务中形成的决策记录。

## 写入规则

- 文档应使用 Markdown。
- 文件名使用小写短横线，例如 `sqlite-retention-runbook.md`。
- 不写入 API key、token、私钥、密码、完整 prompt 或用户隐私。
- 与长期偏好、能力资产、检索记忆相关的内容应写入 `memory/`，不要混入本目录。

## 与 AGENTS.md 的关系

`AGENTS.md` 告诉 Agent 当前项目规则；本目录是 Agent 按这些规则产出的文档目标。
