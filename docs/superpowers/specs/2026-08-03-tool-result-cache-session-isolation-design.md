---
title: 工具结果落盘按会话隔离设计
date: 2026-08-03
status: draft
tags: [runtime, tool-cache, session-isolation, p1-followup]
related:
  - "[[2026-08-01-tool-result-truncation-governance-design]]"
  - "[[truncation-governance-progress]]"
---

# 工具结果落盘按会话隔离设计

## 1. 背景

P1（统一落盘分页）把超预算工具结果落盘到 `toolRoot/.stardust/tool_results/<tool>-<sha256(content)[:10]>.md`（`internal/runtime/toolcache.go writeToolResultCache`）。文件名**只含 tool 名 + 内容 hash，无 session/task 维度**。

**问题（核实见 session-1785754563455689700）**：
- 不同会话抓同一 URL → 同内容 → **同文件名**，落盘目录里所有会话的缓存混在一起。
- **跨会话数据未隔离**：会话 B 的 agent 与会话 A 共享同一 sandbox root，`list_files`/`read_file`/`search_content` 能看到会话 A 遗留的落盘文件。
- **无法按会话清理**：会话删除时清不掉它的落盘。
- 目录随会话无限累积。

> 说明：content hash 命名本身**同内容幂等**（同 URL→同 hash→覆盖但内容一致，无害），所以本设计解决的是**跨会话隔离与可清理性**，不是"防覆盖"。session-…689700 的 `read_file` 失败 3 次疑另有原因（offset 越界等），不在本设计范围。

## 2. 目标与范围

- 落盘路径按 **session 隔离**：`toolRoot/.stardust/tool_results/<session>/<tool>-<hash>.md`
- `session` = `task.SessionID`，空时 fallback `task.ID`，都空 fallback 固定 `no-session`
- `read_file` 读回契约不变
- **不做主动清理**（YAGNI）——session 子目录累积，清理（会话删除时清 / 定期清）留后续独立工作

## 3. 方案

在 runtime 层把 `appendToolResults` 的 `cacheDir` 从常量改为**运行时按 task 拼接的 session 子目录**。不改 `appendToolResults` / `writeToolResultCache` / `renderToolResultContent` 的签名——`cacheDir` 参数已透传，`filepath.Join` 自动把 session 段并入路径，`guard.Check` 仍验证最终路径在 sandbox 内。

### 组件（`internal/runtime/toolcache.go` 新增 + `runtime.go` 一处改）

```go
// sessionCacheDir returns the tool-result cache dir for a task, isolated by
// session: <defaultToolResultCacheDir>/<sanitized session key>. The session key
// is task.SessionID, falling back to task.ID, then to "no-session" (a task
// always has an ID, so the last fallback is a guard against the impossible, not
// a silent default).
func sessionCacheDir(task domain.Task) string {
	key := strings.TrimSpace(task.SessionID)
	if key == "" {
		key = strings.TrimSpace(task.ID)
	}
	if key == "" {
		key = "no-session"
	}
	return filepath.Join(defaultToolResultCacheDir, sanitizeCacheSegment(key))
}

// sanitizeCacheSegment keeps only filename-safe chars from a path segment
// (session/task id). IDs are like "session-1785…"/"gui-task-…" (already safe);
// this is a defensive guard, mirroring sanitizeToolName.
func sanitizeCacheSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "session"
	}
	return out
}
```

`runtime.go` `runToolLoop` 的调用点（当前 runtime.go:516）：
```go
// 前
st.convo.appendToolResults(calls, results, r.maxToolResultChars, r.toolRoot, defaultToolResultCacheDir, r.logger)
// 后
st.convo.appendToolResults(calls, results, r.maxToolResultChars, r.toolRoot, sessionCacheDir(task), r.logger)
```

`task` 已在 `runToolLoop` 签名内可见。

## 4. 路径结构

`toolRoot/.stardust/tool_results/<sanitize(sessionID | taskID | "no-session")>/<tool>-<sha256(content)[:10]>.md`

例：`.stardust/tool_results/session-1785754563455689700/fetch_url-de862b17af.md`

## 5. 隔离效果与 read_file 读回

- 会话 A/B 各自子目录，互不可见/不混。
- **read_file 读回不变**：`writeToolResultCache` 返回相对 `toolRoot` 的路径（`filepath.Rel(absRoot, target)` + `ToSlash`），现在自然含 `<session>/` 段；footer 里的 `read_file path="…"` 仍相对 `toolRoot` 解析（`readFileTool` 用 `resolveToolPath(root, path)`），读到同一文件。offset 语义不变。

## 6. 清理

首版**不主动清**（YAGNI）。session 子目录累积，后续单独做（会话删除时删 `tool_results/<session>` 目录，或定期清 N 天前）。本 spec 不含清理实现。

## 7. 错误处理（fail-loud 铁律）

- session/task id 都空是"本不该发生"（task 总有 ID）→ fallback `no-session`，非静默兜底（有明确落点，可诊断）。
- `sanitizeCacheSegment` 结果为空 → fallback `session`（复用 `sanitizeToolName` 的兜底模式）。
- `guard.Check` 仍对最终 `<session>/<tool>-<hash>.md` 路径验证 sandbox 归属；session 段是内部生成的安全字符串（非用户输入），逃逸不可达但仍走 guard。
- 落盘失败降级（Warn + 纯截断）逻辑不变。

## 8. 测试

- `sessionCacheDir`：SessionID 非空 → `tool_results/<session>`；SessionID 空 + ID 非空 → `tool_results/<taskID>`；都空 → `tool_results/no-session`。
- `sanitizeCacheSegment`：含非法字符 → 替换为 `-`；全非法 → `session`。
- 集成：两个不同 SessionID 的 task 落盘到**不同子目录**（隔离断言）；同 session 多 task 落盘到**同一子目录**。
- read_file 端到端：落盘到 session 子目录后，footer 的相对路径能被同 registry `read_file` 读回（那个坑的回归）。
- `go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 空。

## 9. 实现锚点

- `internal/runtime/toolcache.go`：`defaultToolResultCacheDir` 常量、`writeToolResultCache`、`sanitizeToolName`（新增 `sessionCacheDir` / `sanitizeCacheSegment` 于此，需 import `path/filepath`、`strings`、`github.com/stardust/legion-agent/internal/domain`）。
- `internal/runtime/runtime.go:516`：`runToolLoop` 里 `appendToolResults` 调用点，`defaultToolResultCacheDir` → `sessionCacheDir(task)`。
- `domain.Task.SessionID`（`internal/domain/types.go:66`）。
- 现有测试 `internal/runtime/messages_test.go` / `toolcache_test.go` 的 `appendToolResults` 调用传常量 cacheDir，**无需改**（它们测 render/append 逻辑，不涉 session 拼接）。

## 10. 开放问题

无（隔离粒度=session、空 fallback=task_id、首版不清 均已确认）。
