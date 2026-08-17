---
id: "design-generated-files-capture-001"
title: "生成文件捕获·服务·落盘技术 spec（子项目 A：后端捕获 write_file 产物 + 文件端点 + 可配置 base URL）"
aliases: ["生成文件捕获", "generated files capture", "文件服务端点", "write_file 产物落盘"]
type: "design"
category: "superpowers/specs"
tags: ["legionagent", "runtime", "write_file", "conversation-turns", "generated-files", "http", "file-serving", "config", "spec"]
version: "2.0.0"
created: "2026-08-10"
updated: "2026-08-10"
author: "jxncyjq"
status: "draft"
parent: null
children: []
related_docs:
  - id: "design-token-usage-persistence-001"
    relation: "extends"
    path: "./2026-08-10-token-usage-persistence-design.md"
---

# 生成文件捕获·服务·落盘技术 spec（子项目 A）

> 让 legionAgent 记录每任务用 `write_file` 生成的文件、通过 HTTP 端点把它们当**可配置 base URL 的链接**提供、并持久化到 assistant conversation turn，供 GUI（子项目 B）在对话里显示成可点卡片（预览 / 下载 / 复制链接）。**核心不变量：库里存 workspace 相对路径，URL 在响应时按当前配置 base 现拼**——loopback 端口变、改配置成域名、token 轮换，老卡片都不失效；只有磁盘文件真被删才 404。纯 legionAgent 后端。

<!-- @section: overview -->
## 概述

### 背景
`write_file` 把内容写进会话 `workingDir`，是生成文件的机制。但 `taskResultResponse` 只回文本+token，前端无从得知任务产生了哪些文件、也没有可访问的链接。本 spec 补齐"捕获 → 服务 → 落盘 → 出口"整条链。

### 目标
1. 捕获每任务 `write_file` 产生的相对路径。
2. 新增文件服务 HTTP 端点，按 session 的 workingDir 根限定地流式返回文件（预览 / 下载）。
3. base URL 可配置（loopback 现用；部署填域名）。
4. 任务结果 + 持久化的 assistant turn 里给前端**现拼的完整链接**（存路径、拼 URL）。

### 非目标
- ❌ GUI 卡片渲染、预览、下载、复制链接（= 子项目 B）
- ❌ 域名部署下的持久鉴权（签名 URL / 会话鉴权，替代重启即轮换的 loopback token）——**留作后续服务级 auth 话题**；本 spec 端点沿用现有 /v1 loopback token
- ❌ 捕获 `write_file` 以外的文件产物（当前仅此一个写文件工具）
- ❌ 存文件内容快照（只记路径，文件留在磁盘）

<!-- @section: architecture -->
## 架构

```
runtime loop: write_file 成功 ──> st.generatedFiles (去重, workspace-相对)
                                        │
finishRun: task_completed RuntimeEvent{..., GeneratedFiles}  +  TaskRun.GeneratedFiles
                                        │
server.taskResult 读事件 ──> (result, usage, generatedFiles[])
                                        │
       ┌────────────────────────────────┼───────────────────────────────┐
       ▼                                ▼                               ▼
taskResultResponse.generated_files   recordAssistantTurn           (URL 现拼:
  = [{path, url}]  (url=拼)            存 path[] 进 turn JSON 列       fileURL(base, sid, path))
                                        │
                    ListConversationTurns 读 path[] ──> 出口再拼 url ──> GUI(子项目 B)

新端点: GET /v1/files?session_id=<sid>&path=<rel>[&download=1]
  → 查 session.WorkingDir 作 root → 根限定 → 流式返回(Content-Type 按扩展名; download 时 attachment)
```

复用 token 落盘（[[design-token-usage-persistence-001]] PR #75）同一通道（RuntimeEvent token 字段 → taskResult → recordAssistantTurn）。

<!-- @section: capture -->
## 捕获（internal/runtime/runtime.go）

工具循环（~900，`result, err := r.dispatchToolCall(...)` **成功**分支）后：若 `call.Name == "write_file"`，取 `call.Arguments["path"]`（`ToolCall.Arguments` 是 `map[string]string`），规范化为 workspace-root 相对路径，追加进 `loopState.generatedFiles`（`[]string`，**去重**、保写入顺序）。
- `loopState`（`st`）加 `generatedFiles []string`。
- 规范化：path 可能相对或 workspace 内绝对；统一转相对 root（`filepath.Rel` / 现有 workspace path guard）。失败 → fail-loud 返回 error（不静默丢）。
- `Arguments["path"]` 缺失但工具报成功 = 不变量违反 → fail-loud（write_file 契约必有 path）。
- 只在成功时收（失败分支已 `continue`）。

<!-- @section: config -->
## 可配置 base URL（internal/config/config.go）

`ServerConfig`（~102）加字段：
```go
// FileBaseURL is the public base for generated-file links (no trailing slash),
// e.g. "https://agent.example.com". Empty (default) means use the runtime
// loopback origin "http://127.0.0.1:<port>". Deployment sets a domain here.
FileBaseURL string `json:"file_base_url,omitempty"`
```
- 契约可选：空 = 用运行时 loopback origin（合法缺省，非兜底，按 ADR-0001 可选槽）。
- 加载/校验沿用现有 config 管线；非空时应是合法 URL 前缀（做基本校验，非法则启动 fail-loud）。

<!-- @section: endpoint -->
## 文件服务端点（internal/server/http.go）

在路由 switch（~268）加：
```
case GET  r.URL.Path == "/v1/files":  // query: session_id, path, download(可选)
```
- 鉴权：沿用现有 loopback token（`Authorization: Bearer <adminToken>`，见 ~338）。
- 解析 `session_id` → 查该 session 的 `WorkingDir` 作 root（session store）。空 workingDir → 404/400（无根不可服务）。
- `path` 根限定：`filepath.Rel(root, clean(join(root,path)))` 不得越出 root，否则 403（越权）。
- 存在性：文件不存在/非普通文件 → 404（fail-loud，"文件不存在/已移动"）。
- 响应：`Content-Type` 按扩展名（`mime.TypeByExtension`，兜底 `application/octet-stream`——这是 MIME 契约缺省，非逻辑兜底）；`?download=1` 加 `Content-Disposition: attachment; filename="<base>"`；用 `http.ServeContent`/`io.Copy` 流式，不整块读进内存（大文件友好）。
- 只读；不接受写。

### URL 拼装 helper
```go
// fileURL builds a generated-file link from the configured base (or loopback
// fallback) + session + relative path. Never persisted — always built fresh so
// port/domain/token changes never invalidate stored cards.
func (s *HTTPServer) fileURL(sessionID, relPath string, download bool) string
```
base = `cfg.FileBaseURL` 非空则用之，否则 `http://127.0.0.1:<当前port>`；path/session 走 query 编码。

<!-- @section: transport-surface -->
## 传输与出口

- `domain.RuntimeEvent` 加 `GeneratedFiles []string json:"generated_files,omitempty"`；`domain.TaskRun` 加 `GeneratedFiles []string`。
- `finishRun`（runtime.go ~737 task_completed 事件 + 同处 TaskRun）填 `st.generatedFiles`。
- `taskResult`（http.go ~1102）签名多返回 `generatedFiles []string`（读 `event.GeneratedFiles`）。
- `taskResultResponse`（~991）加：
```go
GeneratedFiles []GeneratedFile `json:"generated_files"` // 空任务 = []，非 null
```
```go
type GeneratedFile struct {
    Path        string `json:"path"`          // workspace 相对
    URL         string `json:"url"`           // 现拼: fileURL(sid, path, false)
    DownloadURL string `json:"download_url"`  // 现拼: fileURL(sid, path, true)
    Name        string `json:"name"`          // filepath.Base(path)
}
```
- `recordAssistantTurn`（~1072，已接 `usage taskUsage`）加参 `generatedFiles []string`，写进 turn。

<!-- @section: persistence -->
## 落盘（internal/storage/sqlite.go + domain）

- `domain.ConversationTurn` 加 `GeneratedFiles []string`（存**相对路径**；URL 由 server 出口现拼，不入库）。
- Schema：`conversation_turns` 加 `generated_files TEXT NOT NULL DEFAULT '[]'`（JSON 数组）。CREATE（~1892）+ `columnMigrations` 幂等 ALTER（~1640）双写。
- 写：`AppendConversationTurn`（~400）+ `AppendConversationTurnIfAbsent`（~435）INSERT 加列，值 `json.Marshal(turn.GeneratedFiles)`（nil/空 → `[]`）。
- 读：`ListConversationTurns` SELECT（~467）加列；`scanConversationTurn`（~1757）scan 到 string → `json.Unmarshal`。
- 历史会话读回接口（GUI 加载历史）应把相对路径**现拼成 URL** 再给前端（复用 `fileURL`）——与任务结果出口一致。
- 编解码 fail-loud：Marshal/Unmarshal error 包装返回，不 `_ = err`、不空数组冒充。

<!-- @section: failloud -->
## fail-loud（守 legionAgent CLAUDE.md 铁律）

- 空生成列表 = `[]`（没写文件）、`FileBaseURL` 空 = loopback：均为**契约允许的可选**，非兜底。
- path 规范化失败、`Arguments["path"]` 缺失却报成功、JSON 编解码错、非法 FileBaseURL：一律 fail-loud（返回/记录 error 或启动 Fatal）。
- 文件端点：无 session workingDir、越权 path、文件不存在 → 对应 4xx，不静默返回空。
- `mime.TypeByExtension` 未识别 → `application/octet-stream`（MIME 标准缺省，属契约可选）。

<!-- @section: testing -->
## 测试

- **捕获**（runtime）：任务内两次 write_file（含重复 path）→ `st.generatedFiles` 去重、保序；失败/非 write_file 不计入；path 缺失报成功 → 返回 error。
- **config**：`FileBaseURL` 空 → loopback；非空合法 → 用之；非法 → 启动 fail-loud。
- **端点**（server）：正常 session+path → 200 + 正确 Content-Type；`download=1` → attachment；越权 path → 403；缺文件 → 404；无 workingDir → 4xx；无/错 token → 401。
- **URL 拼装**：`fileURL` 用配置 base；空配置回退 loopback + 当前 port；query 编码正确。
- **传输/出口**：task_completed 带 GeneratedFiles；`taskResultResponse.generated_files` = `[]`（空任务）非 null，含 path/url/download_url/name。
- **落盘往返**（storage）：写带 generated_files 的 assistant turn → 读回一致；user turn 读回 `[]`；迁移（新库 CREATE + 老库 ALTER 幂等）有列。
- **全绿**：`go build ./... && go vet ./... && go test ./...`、`gofmt -l .` 空；错误路径有断言。

<!-- @section: scope -->
## 范围与工作量
**改动**：`internal/config/config.go`（FileBaseURL + 校验）；`internal/domain/types.go`（RuntimeEvent/TaskRun/ConversationTurn 加字段 + GeneratedFile DTO 放 server）；`internal/runtime/runtime.go`（捕获 + 填值）；`internal/server/http.go`（/v1/files 端点 + fileURL + taskResult 返回 + taskResultResponse + recordAssistantTurn 加参 + 历史读出拼 URL）；`internal/storage/sqlite.go`（schema + INSERT×2 + SELECT + scan + JSON）。
**仓库**：仅 legionAgent。

<!-- @section: open-questions -->
## 待确认 / 后续
- **子项目 B（GUI）**：读 `generated_files` → 对话卡片；预览（html/md/文本/代码/图片经端点 URL 或 ReadWorkspaceFile → PreviewContent）、下载（download_url）、复制链接（url）。office（docx/xlsx/pptx）走下载/外部打开。
- **域名部署持久鉴权**：loopback token 每次重启轮换,不适合域名分享;签名 URL / 会话鉴权是独立的服务级 auth spec。
- 大文件/并发流式的限流、Range 支持（`http.ServeContent` 原生支持 Range，够用）。
