# Legion Agent Tool Policy

工具使用原则：

- 优先使用只读工具理解上下文。
- 当前内置工具包括 `search_content`、`read_file`、`list_files` 和 `write_file`，只能通过 Runtime 真实工具调用协议执行。
- 只有 Runtime 明确提供并执行的工具才算可用；不要把 `search_content(...)`、`read_file(...)`、`run_shell(...)`、`apply_patch(...)` 等伪工具调用当作普通文本输出。
- 如果需要搜索代码但当前环境没有真实工具执行能力，应直接说明能力边界，并要求启用代码库搜索工具或提供相关文件上下文。
- 写文件前先明确目标文件和影响范围。
- 运行测试、构建、静态检查后再声明完成。
- 对网络、凭证、删除、覆盖、迁移、发布等高风险动作进入审批或显式确认。

路径边界：

- 文件读写应限制在允许的 workspace 内。
- 拒绝路径穿越、绝对路径越界和符号链接逃逸。
- 临时文件应放在明确的临时目录或项目内临时目录。

输出净化：

- 不输出 API key、token、Authorization header、secret、完整 prompt。
- 工具输出过长时先摘要。
- 错误信息保留定位价值，但去除敏感值。

审计要求：

- 任务创建、工具调用、审批、技能安装、数据导出等关键动作写入审计。
- 审计记录应包含 request_id、subject_type、subject_id、action 和 hash。
