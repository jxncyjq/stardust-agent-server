---
title: read_file 重读可见提醒（approach C）设计
date: 2026-07-28
status: approved
scope: legionAgent — 任务内重复整篇读取同一文件时，前置显眼提醒，引导改用 search_content
---

# read_file 重读可见提醒设计

## 目标

减少多轮 tool-loop 因**模型反复整篇重读同一文件**导致的 token 爆炸（实测「员工在上海」5 字追问跑 13 轮、手册 4024 字被读约 5 次、累计 166k 字符≈95.7k token）。做法：read_file 读到与本任务前次**相同内容**时，在结果前置显眼提醒（含第 N 次计数 + 引导 search_content），全文仍返回。让模型看见自己在重复、主动停手，轮数下降。

## 背景与硬约束

`internal/runtime/messages.go` 的 `conversation` 是**故意 append-only**：早期按 (name,args) 折叠重复 tool 结果，使每轮 prompt 字节相同、模型看不出已读过 → 2026-07-23 一个任务 152 次相同读、554s（见 memory `legion-tool-loop-multiturn`）。故本设计的三条铁约束：
1. **内容永不丢**（fail-loud）——重读仍返回全文，只是前置提醒。
2. **重复对模型可见**——提醒含「第 N 次」，各轮不同，绝不折叠成字节相同。
3. **不减 render 语义**——`render(maxChars)` 的按轮裁剪不动；本设计只加提醒文本。

真实成本结构：`render` 已把超 16k 的旧 tool 输出裁成 stub，单轮封顶 ~16k；13 轮×16k=166k 的大头是**轮数**。轮数多源于模型重读，故降 token 的杠杆 = 让模型停止重读（减轮），而非单轮去重。

## 决策（已确认，approach C）

1. **触发键 = 路径 + 内容哈希**：同一 task 内，read_file 对同一解析路径再次读取，且返回内容哈希与前次**相同** → 提醒；内容**变化** → 不提醒（合法轮询/新内容，不误伤）。
2. **仅 read_file**：search_content（命中行）、list_files（目录）不动。
3. **任务级状态**：`readHistory`（mutex + `map[path]{hash,count}`），每 registry 一个（registry 每任务新建，天然 task 隔离），仿现有 `injectedAgentsSet`。
4. **提醒前置、全文在后**：不改动/不截断内容本身；与现有 agents.md 注入（`subtreeAgentsNote`）叠加，顺序 = 提醒 + 全文 + agents 注入。

## 架构

### 组件（`internal/tool/builtin.go`）
- `type readHistory struct { mu sync.Mutex; seen map[string]readEntry }`；`type readEntry struct { hash string; count int }`。
- `func newReadHistory() *readHistory`。
- `func (h *readHistory) record(path, content string) (count int, unchanged bool)`：加锁；`count = seen[path].count + 1`；`unchanged = count > 1 && hash(content) == seen[path].hash`；写回 `{hash, count}`；返回。哈希用 `crypto/sha256`（或 `hash/fnv`，内容完整性判等即可，非安全用途）。
- `workspaceRegistryOptions` 加 `readHistory *readHistory`。
- 在 workspace registry 构造器（`NewWorkspaceRegistry` / `NewFileReadWriteWorkspaceRegistry`）内 `options.readHistory = newReadHistory()`（始终非 nil，避免 nil 判分支）。
- `readFileTool` 拿到 `content`（截断后、追加 agents 注入前）：`count, unchanged := options.readHistory.record(resolvedPath, content)`；`if unchanged { output = repeatNotice(count) + output }`。
- `func repeatNotice(count int) string` 返回固定文案（含 count）。

### 提醒文案
```
⚠️ 本任务已第 <N> 次读取此文件，内容与前次相同、未变化。该内容此前已在上文出现；
若只需其中某段，请改用 search_content 精确检索，避免重复整篇读取消耗上下文。

```
（末尾空行分隔全文。）

## 数据流

1. round k：read_file X → 首读，record 返回 (1, false) → 无提醒，全文。
2. round k+2：read_file X（内容未变）→ record 返回 (2, true) → 前置「第 2 次」提醒 + 全文。
3. 模型见提醒 → 改用 search_content 或停止重读 → 轮数下降。
4. 若 X 内容在两次读之间变化：record 返回 (2, false) → 无提醒（新内容照常全文）。

## 错误处理（fail-loud）

- 内容哈希不涉及外部依赖，不产生 error；`record` 无 error 返回。
- 绝不因去重逻辑吞掉或截断真实内容——`unchanged` 仅决定是否**前置提醒**，全文恒返回。
- `readHistory` 并发安全（mutex），覆盖并行 tool 调用。
- 未启用（demo 等）registry 也构造 readHistory（非 nil），无分支坑；作用无害（demo 少读）。

## 测试（`internal/tool/builtin_test.go`）

- 同文件二读、内容相同 → 第二次 output 含提醒 + 「第 2 次」，且全文完整仍在。
- 同文件二读、其间内容改变 → 第二次**无**提醒，返回新全文。
- 两个不同文件各读一次 → 均无提醒。
- 三读同文件 → 计数递增（第 2、第 3 次都有提醒且 N 正确）。
- `readHistory.record` 单元测试：count 递增、unchanged 判定（首读 false、同内容重读 true、变内容 false）。
- 门禁：`go build/vet/test ./...` 全绿、`gofmt -l .` 空。

## 非目标

- 不动 `render`/`conversation` 的 append-only 与按轮裁剪逻辑。
- 不对 search_content / list_files 加提醒。
- 不做 max_tool_rounds 硬限（用户明确暂不加）。
- 不做跨任务的读缓存（每任务隔离）。

## 相关

memory：[[legion-tool-loop-multiturn]]（152 次事故 = 本设计的反面教材与硬约束来源）、[[legion-token-multiround-debug-probe]]（探针证实的多轮累积成本结构）。
