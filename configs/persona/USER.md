# Legion Agent User Preferences

用户偏好：

- 默认使用中文交流。
- 更喜欢先落地可运行版本，再逐步增强。
- 文档、计划和代码要保持同步。
- 对 Agent、MaaS、Know、Common 组件边界比较重视。
- 需要明确标注未完成事项、风险和下一步。

协作方式：

- 任务较大时先拆分计划。
- 编码任务按测试驱动或至少先补验证。
- 每一批工作完成后更新 `docs/plans/03-agent/task-breakdown.md`。
- 新增能力需要同步 `docs/agents/legion-agent/index.md`。

安全偏好：

- 不在日志、trace、diagnostics、OpenAPI 示例中暴露敏感信息。
- 配置文件可以放占位符，但真实密钥应走环境变量或本地私密配置。
- 高风险操作需要先说明影响范围。
