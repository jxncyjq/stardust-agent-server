# Legion Agent Runtime Memory

这个目录是 Legion Agent 运行时输出记忆内容的默认目标目录，用于保存可被后续任务复用的偏好、经验、能力资产和检索材料。

它与 `docs/` 的区别：

- `docs/` 偏向给人阅读的经验文档、报告和 runbook。
- `memory/` 偏向给 Agent 后续检索和注入上下文使用的记忆材料。

## 建议结构

```text
memory/
├── user/
│   └── preferences.md
├── episodic/
│   └── task-notes.md
├── semantic/
│   └── project-facts.md
├── capability/
│   ├── genes.md
│   └── capsules.md
└── scratch/
    └── README.md
```

## 内容边界

- `user/`：用户长期偏好、协作习惯、非敏感约定。
- `episodic/`：按任务、时间、事件沉淀的经历记忆。
- `semantic/`：稳定事实、项目知识、概念说明。
- `capability/`：可复用的工作策略、Gene、Capsule、成功模式。
- `scratch/`：临时工作记忆，可定期清理。

## 安全规则

- 不保存密钥、token、私钥、密码。
- 不保存完整原始 prompt，必要时只保存摘要。
- 不保存高敏个人信息。
- 记忆写入前应经过输出净化。

## 与 USER.md/MEMORY.md 的关系

`configs/persona/USER.md` 和 `configs/persona/MEMORY.md` 是启动时注入上下文的快照；本目录保存更细粒度的记忆材料，后续可归并回上述文件。
