# 生成文件捕获·服务·落盘 Implementation Plan（子项目 A）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** legionAgent 捕获每任务 `write_file` 生成的文件、通过可配置 base URL 的 HTTP 端点提供、并持久化到 assistant conversation turn，供 GUI（子项目 B）显示文件卡片。

**Architecture:** 捕获（runtime）→ 搭 task_completed 事件车 → taskResult 出口 → `taskResultResponse.generated_files`(含现拼 url) + 落 assistant turn(存相对路径 JSON)。新增 `GET /v1/files` 端点按 session workingDir 根限定流式返回。base URL 可配置：配了域名返回绝对 URL，空则返回相对路径(GUI 用已知 baseURL 前缀拼)。库里只存相对路径，URL 现拼——端口/域名/token 变都不失效。纯 legionAgent 后端。

**Tech Stack:** Go + SQLite + net/http。守 legionAgent CLAUDE.md fail-loud 铁律。

**Spec:** `docs/superpowers/specs/2026-08-10-generated-files-capture-design.md`

**仓库根：** 相对 `legion/legionAgent/`。校验：`go build ./... && go vet ./... && go test ./...`、`gofmt -l .`（在该目录跑）。storage 测试 helper=`openTestRepo(t)`。

---

## 文件结构

| 文件 | 改动 |
|---|---|
| `internal/config/config.go` | `ServerConfig` 加 `FileBaseURL` + 校验 |
| `internal/domain/types.go` | `RuntimeEvent`/`TaskRun`/`ConversationTurn` 各加 `GeneratedFiles []string` |
| `internal/runtime/runtime.go` | 循环捕获 write_file path;`finishRun` 填 event+TaskRun |
| `internal/storage/sqlite.go` | `conversation_turns` 加 `generated_files` JSON 列(schema+迁移+INSERT×2+SELECT+scan) |
| `internal/server/http.go` | `/v1/files` 端点 + `fileURL` helper + `taskResult` 返回 + `taskResultResponse`+`GeneratedFile` DTO + `recordAssistantTurn` 加参 + 历史读出拼 URL |
| 各 `*_test.go` | 测试 |

---

### Task 1: config — FileBaseURL

**Files:** `internal/config/config.go`；`internal/config/config_test.go`（追加）

- [ ] **Step 1: 失败测试** — 断言 `FileBaseURL` 反序列化;空默认"";非法 URL(非 http/https 前缀)Load 报错。先读 `config_test.go` 现有用例与 `Load` 校验风格,对齐。
```go
func TestServerConfigFileBaseURL(t *testing.T) {
	// 用现有测试构造 Config 的方式：非空合法 URL 应保留，空应为 ""
	// 非法（如 "not a url"）应在 Load/Validate 阶段返回 error
}
```
（补全成完整用例：一个 happy `"https://agent.example.com"` 保留、一个空为默认、一个 `"ftp://x"` 或 `"nonsense"` 触发 Load error。参照现有 config 校验测试写法。）

- [ ] **Step 2: 确认失败** `go test ./internal/config/ -run FileBaseURL`.

- [ ] **Step 3: 实现**
  - `ServerConfig`（~102）加：
```go
	// FileBaseURL is the public base for generated-file links (no trailing slash),
	// e.g. "https://agent.example.com". Empty (default) means links are returned
	// as relative paths ("/v1/files?..."), which the loopback frontend resolves
	// against its own known base URL. Deployment sets a domain here.
	FileBaseURL string `json:"file_base_url,omitempty"`
```
  - 在现有 `Load`/`Validate` 校验处（找 ServerConfig 校验段）：`FileBaseURL` 非空时校验以 `http://` 或 `https://` 开头且能 `url.Parse`，否则 `fmt.Errorf("server.file_base_url %q invalid: %w", ...)`。空跳过（契约可选）。去尾部 `/`（规整）。

- [ ] **Step 4: 确认通过** `go test ./internal/config/ -v -run FileBaseURL`;`gofmt -l internal/config/config.go`（空）。

- [ ] **Step 5: 提交**
```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add server.file_base_url for generated-file links"
```

---

### Task 2: domain — GeneratedFiles 字段

**Files:** `internal/domain/types.go`

- [ ] **Step 1: 实现** — 三个结构各加字段（纯增补）：
  - `RuntimeEvent`（~有 token 字段那个）：`GeneratedFiles []string \`json:"generated_files,omitempty"\``
  - `TaskRun`：`GeneratedFiles []string \`json:"generated_files,omitempty"\``
  - `ConversationTurn`（已有 token 字段）：`GeneratedFiles []string \`json:"generated_files,omitempty"\``（存 workspace 相对路径）

- [ ] **Step 2: 确认编译** `go build ./... && gofmt -l internal/domain/types.go`（空）。

- [ ] **Step 3: 提交**
```bash
git add internal/domain/types.go
git commit -m "feat(domain): add GeneratedFiles to RuntimeEvent/TaskRun/ConversationTurn"
```

---

### Task 3: runtime — 捕获 write_file

**Files:** `internal/runtime/runtime.go`；`internal/runtime/*_test.go`

**Context:** 工具循环（~900）成功分支后收 write_file 的 path;`finishRun`（~737 task_completed 事件 + 同处 TaskRun）填值。`loopState`（`st`）加 `generatedFiles []string`。`ToolCall.Arguments` 是 `map[string]string`,path=`Arguments["path"]`。root = task 的 workspace root(用 finishRun/loop 已有的 workingDir/guard;若无现成则用 `task.WorkingDir`)。

- [ ] **Step 1: 失败测试** — 找 runtime 里驱动完整 tool-loop 的测试(用 fake tool runner/registry)。断言：任务内 write_file 两次(一次重复 path)→ `finishRun` 写出的 task_completed 事件 `GeneratedFiles` 去重保序;write_file 失败不计入;非 write_file 不计入。若现有测试设施难驱动,写窄测试：构造带若干 write_file 结果的 `st`,调 `finishRun`,断言 fake 事件总线收到的 task_completed 事件 GeneratedFiles 正确。先 `grep -rn "generatedFiles\|write_file\|dispatchToolCall\|task_completed\|loopState" internal/runtime/*_test.go` 找现有断言点。报告采用的方式。

- [ ] **Step 2: 确认失败**。

- [ ] **Step 3: 实现**
  1. `loopState` 加 `generatedFiles []string`。
  2. 工具循环成功分支(`results = append(results, result)` 附近,~915)后加：
```go
		if call.Name == "write_file" {
			raw, ok := call.Arguments["path"]
			if !ok || strings.TrimSpace(raw) == "" {
				// write_file 契约必有 path；报成功却无 path = 不变量违反
				return nil, fmt.Errorf("write_file for task %s reported success without a path argument", task.ID)
			}
			rel, err := workspaceRelPath(task.WorkingDir, raw) // 见下 helper
			if err != nil {
				return nil, fmt.Errorf("normalize generated file %q for task %s: %w", raw, task.ID, err)
			}
			st.generatedFiles = appendUniqueStr(st.generatedFiles, rel)
		}
```
  3. helper（同包）：
```go
// workspaceRelPath normalizes a write_file path (relative or absolute-within-root)
// to a slash path relative to root. root=="" (session unbound) falls back to the
// path cleaned as-is. Returns error if the result escapes root.
func workspaceRelPath(root, p string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return filepath.ToSlash(filepath.Clean(p)), nil
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, p)
	}
	rel, err := filepath.Rel(root, filepath.Clean(abs))
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace root", p)
	}
	return filepath.ToSlash(rel), nil
}

func appendUniqueStr(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}
```
  4. `finishRun`：task_completed `RuntimeEvent`（~737）加 `GeneratedFiles: st.generatedFiles`；同处 `domain.TaskRun` 加 `GeneratedFiles: st.generatedFiles`。
  （确认 `strings`/`filepath` 已 import。）

- [ ] **Step 4: 确认通过** `go test ./internal/runtime/ -v`（相关用例）;`gofmt -l internal/runtime/runtime.go`（空）;`go vet ./...`。

- [ ] **Step 5: 提交**
```bash
git add internal/runtime/runtime.go internal/runtime/
git commit -m "feat(runtime): capture write_file paths into task GeneratedFiles"
```

---

### Task 4: storage — conversation_turns generated_files 列

**Files:** `internal/storage/sqlite.go`；`internal/storage/sqlite_generated_files_test.go`

**Context:** 与 token 列同款(参照已合入的 token 列改法)。加 `generated_files TEXT NOT NULL DEFAULT '[]'`(JSON 数组);INSERT 存 `json.Marshal`,scan `json.Unmarshal`。

- [ ] **Step 1: 失败测试**：
```go
func TestConversationTurnGeneratedFilesRoundTrip(t *testing.T) {
	r := openTestRepo(t)
	ctx := context.Background()
	turn := domain.ConversationTurn{
		ID: "t1:assistant", SessionID: "s1", TaskID: "t1", AgentID: "a1",
		Role: domain.ConversationRoleAssistant, Content: "hi",
		GeneratedFiles: []string{"docs/a.html", "out/b.md"},
		CreatedAt: time.Now(),
	}
	if err := r.AppendConversationTurn(ctx, turn); err != nil { t.Fatal(err) }
	turns, err := r.ListConversationTurns(ctx, "s1", 0)
	if err != nil { t.Fatal(err) }
	if len(turns) != 1 || len(turns[0].GeneratedFiles) != 2 || turns[0].GeneratedFiles[0] != "docs/a.html" {
		t.Fatalf("got %+v", turns)
	}
}

func TestConversationTurnNoGeneratedFilesReadsEmpty(t *testing.T) {
	r := openTestRepo(t)
	ctx := context.Background()
	turn := domain.ConversationTurn{ID: "t2:user", SessionID: "s2", TaskID: "t2", AgentID: "a1", Role: domain.ConversationRoleUser, Content: "q", CreatedAt: time.Now()}
	if _, err := r.AppendConversationTurnIfAbsent(ctx, turn); err != nil { t.Fatal(err) }
	turns, _ := r.ListConversationTurns(ctx, "s2", 0)
	if len(turns) != 1 || len(turns[0].GeneratedFiles) != 0 {
		t.Fatalf("want empty, got %+v", turns)
	}
}
```
（若需前置 session 行,照现有 turn 测试 seed。）

- [ ] **Step 2: 确认失败** `go test ./internal/storage/ -run GeneratedFiles`.

- [ ] **Step 3: 实现**
  - CREATE `conversation_turns`（~1892）加：`generated_files TEXT NOT NULL DEFAULT '[]'`。
  - `columnMigrations`（~1640）加：`{table:"conversation_turns", column:"generated_files", stmt:\`ALTER TABLE conversation_turns ADD COLUMN generated_files TEXT NOT NULL DEFAULT '[]'\`}`。
  - `AppendConversationTurn`（~400）+ `AppendConversationTurnIfAbsent`（~435）INSERT 加 `generated_files` 列 + 占位;值：
```go
	gf, err := json.Marshal(turn.GeneratedFiles)
	if err != nil {
		return fmt.Errorf("marshal generated files for turn %q: %w", turn.ID, err)
	}
	// 若 turn.GeneratedFiles 为 nil,json.Marshal 得 "null" —— 用 "[]" 代替保稳定：
	if turn.GeneratedFiles == nil { gf = []byte("[]") }
	// ...INSERT 传 string(gf)
```
  （`AppendConversationTurnIfAbsent` 返回 `(bool,error)`,同样处理。）
  - `ListConversationTurns` SELECT（~467）加 `generated_files` 列(接在 token 列后)。
  - `scanConversationTurn`（~1757）加一个 `var gfRaw string` scan 目标(接在 token 目标后),然后：
```go
	if err := json.Unmarshal([]byte(gfRaw), &turn.GeneratedFiles); err != nil {
		return domain.ConversationTurn{}, fmt.Errorf("unmarshal generated files for turn %q: %w", turn.ID, err)
	}
```
  列顺序 INSERT/SELECT/scan 三处严格同步(接在 total_tokens 之后)。

- [ ] **Step 4: 确认通过** `go test ./internal/storage/ -run GeneratedFiles -v` + 全包回归 `go test ./internal/storage/`;`gofmt -l internal/storage/sqlite.go`（空）。

- [ ] **Step 5: 提交**
```bash
git add internal/storage/sqlite.go internal/storage/sqlite_generated_files_test.go
git commit -m "feat(storage): persist generated_files JSON on conversation turns"
```

---

### Task 5: server — /v1/files 端点 + fileURL

**Files:** `internal/server/http.go`；`internal/server/*_test.go`

**Context:** 新 GET 端点按 session workingDir 根限定流式返回文件。`HTTPServer` 有 `sessions SessionStore`(方法 `GetAgentSession(ctx,id)(domain.AgentSession,bool,error)`,`session.WorkingDir`)、`adminToken`。token 校验：`r.Header.Get("Authorization") == "Bearer "+s.adminToken`(见 ~338 现有用法)。路由 switch 在 ~268。

- [ ] **Step 1: 失败测试** — `internal/server/*_test.go` 加(参照现有 handler 测试构造 HTTPServer + fake sessions store)：
  - 正常 session+存在文件 → 200 + Content-Type 匹配扩展名 + body=文件内容
  - `?download=1` → 有 `Content-Disposition: attachment`
  - path 越权(`../..`) → 403
  - 文件不存在 → 404
  - session 无 workingDir → 400/404
  - 缺/错 token → 401
  先 `grep -rn "newTestServer\|httptest.NewRequest\|func.*HTTPServer.*test\|fakeSession" internal/server/*_test.go` 找现有测试构造法,对齐。报告采用方式。

- [ ] **Step 2: 确认失败**。

- [ ] **Step 3: 实现**
  - 路由 switch（~268）加：
```go
	case r.Method == http.MethodGet && r.URL.Path == "/v1/files":
		s.handleServeFile(rec, r)
```
  - handler：
```go
// handleServeFile streams a generated file for read-only preview/download. It is
// confined to the requesting session's working directory: the caller passes
// session_id + a workspace-relative path, and any path escaping the root is
// refused. Auth reuses the loopback admin token.
func (s *HTTPServer) handleServeFile(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) { // 用现有 token 判定；若无该 helper 则 inline: r.Header.Get("Authorization") != "Bearer "+s.adminToken
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q := r.URL.Query()
	sessionID := strings.TrimSpace(q.Get("session_id"))
	rel := q.Get("path")
	if sessionID == "" || strings.TrimSpace(rel) == "" {
		writeError(w, http.StatusBadRequest, "session_id and path are required")
		return
	}
	session, ok, err := s.sessions.GetAgentSession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	root := strings.TrimSpace(session.WorkingDir)
	if root == "" {
		writeError(w, http.StatusNotFound, "session has no working directory")
		return
	}
	abs, err := resolveInWorkspace(root, rel) // filepath.Rel 校验；越权 → error
	if err != nil {
		writeError(w, http.StatusForbidden, "path outside workspace")
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	defer f.Close()
	ctype := mime.TypeByExtension(filepath.Ext(abs))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	if q.Get("download") == "1" {
		w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(abs)+"\"")
	}
	http.ServeContent(w, r, filepath.Base(abs), info.ModTime(), f) // 支持 Range
}

// resolveInWorkspace joins rel onto root and refuses any path escaping root.
func resolveInWorkspace(root, rel string) (string, error) {
	abs := filepath.Clean(filepath.Join(root, rel))
	rp, err := filepath.Rel(root, abs)
	if err != nil { return "", err }
	if rp == ".." || strings.HasPrefix(rp, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q outside workspace root", rel)
	}
	return abs, nil
}

// fileURL builds a link for a generated file. When FileBaseURL is configured it
// is absolute (deployment/domain); otherwise a relative "/v1/files?..." path the
// loopback frontend resolves against its own base URL. Never persisted.
func (s *HTTPServer) fileURL(sessionID, relPath string, download bool) string {
	v := url.Values{}
	v.Set("session_id", sessionID)
	v.Set("path", relPath)
	if download { v.Set("download", "1") }
	rel := "/v1/files?" + v.Encode()
	if s.fileBaseURL != "" { // 见下：HTTPServer 加字段
		return s.fileBaseURL + rel
	}
	return rel
}
```
  - `HTTPServer` 加字段 `fileBaseURL string`,在构造 HTTPServer 处从 `ServerConfig.FileBaseURL` 注入(找 `Config`(http.go ~98) → HTTPServer 装配点,把 cfg.FileBaseURL 传进去;`server.Config` 若与 config.ServerConfig 分离,则在装配处桥接)。
  - 若已有 token 判定 helper(如 `s.authorized`/类似)复用之;没有就 inline 判定。`url`/`mime`/`os` 需 import。
  - `writeError` 是现有 helper(前面代码用过)。

- [ ] **Step 4: 确认通过** `go test ./internal/server/ -v`(相关用例) + 回归;`gofmt -l internal/server/http.go`（空）;`go vet ./...`。

- [ ] **Step 5: 提交**
```bash
git add internal/server/http.go internal/server/
git commit -m "feat(server): add GET /v1/files endpoint + fileURL builder"
```

---

### Task 6: server — 出口接线（taskResult / response / turn / 历史）

**Files:** `internal/server/http.go`；`internal/server/*_test.go`

**Context:** 把 generatedFiles 从 task_completed 事件流到 `taskResultResponse` + 落 assistant turn + 历史读出拼 URL。`taskResult`(~1102)现返回 `(result, usage, err)`,`recordAssistantTurn`(~1072)已接 `usage taskUsage`。

- [ ] **Step 1: 失败测试**：
  - task-result 端点响应含 `generated_files: [{path,url,download_url,name}]`,空任务为 `[]` 非 null,url 由 fileURL 拼
  - 完成任务落盘的 assistant turn 带 generatedFiles(相对路径)
  - 历史 turns 读出端点(`ListConversationTurns` 对应的 GET)把相对路径拼成 url 返回
  先找现有 task-result + 历史 turns 端点测试对齐。报告方式。

- [ ] **Step 2: 确认失败**。

- [ ] **Step 3: 实现**
  - `GeneratedFile` DTO（http.go）：
```go
type GeneratedFile struct {
	Path        string `json:"path"`
	URL         string `json:"url"`
	DownloadURL string `json:"download_url"`
	Name        string `json:"name"`
}
```
  - `taskResultResponse`（~991）加 `GeneratedFiles []GeneratedFile \`json:"generated_files"\``。
  - `taskResult`（~1102）签名多返回 `generatedFiles []string`,读 `event.GeneratedFiles`(遍历 task_completed 事件时一并取)。
  - 结果 handler（~1039 调 taskResult 处、~1055 写响应、~1050 调 recordAssistantTurn）：
    - `result, usage, generatedFiles, err := s.taskResult(taskID)`
    - 构造 `[]GeneratedFile`：对每个 rel,`{Path: rel, URL: s.fileURL(task.SessionID, rel, false), DownloadURL: s.fileURL(task.SessionID, rel, true), Name: filepath.Base(rel)}`;写进 response(空则 `[]GeneratedFile{}` 保非 null)
    - `s.recordAssistantTurn(r.Context(), task, result, usage, generatedFiles)`
  - `recordAssistantTurn`（~1072）加参 `generatedFiles []string`,写进 `domain.ConversationTurn.GeneratedFiles`。
  - 历史 turns 读出端点(`handleListConversationTurns` 之类,~427 用 ListConversationTurns 那个)：把每个 turn 的 `GeneratedFiles`(相对路径)映射成 `[]GeneratedFile`(现拼 url)再序列化。**注意**:若该端点直接返回 `domain.ConversationTurn`,需要包一层 DTO 或在序列化前替换——保证前端历史卡片也有 url。找该 handler,加 url 拼装。

- [ ] **Step 4: 确认通过** `go test ./internal/server/ -v` + 回归;`gofmt -l internal/server/http.go`（空）。

- [ ] **Step 5: 提交**
```bash
git add internal/server/http.go internal/server/
git commit -m "feat(server): surface generated_files with links on task result + turns"
```

---

### Task 7: 全量校验

- [ ] **Step 1** `go build ./... && go vet ./... && go test ./...`（全绿）。
- [ ] **Step 2** `gofmt -l .`（本次改动文件应不在列;既有 browser/cli 格式债非本次,忽略）——`gofmt -l $(git diff --name-only master..HEAD | grep '\.go$')` 应为空。
- [ ] **Step 3（可选真机）** 跑一个让 agent write_file 的任务 → `sqlite3 agent.db "SELECT id, generated_files FROM conversation_turns WHERE role='assistant' ORDER BY created_at DESC LIMIT 3;"` 见非空 JSON;`curl -H "Authorization: Bearer <token>" "http://127.0.0.1:<port>/v1/files?session_id=<sid>&path=<rel>"` 能取到文件。
- [ ] **Step 4** 收尾提交（如微调）。

---

## 范围外（后续）
- **子项目 B（GUI）**：对话文件卡片(预览/下载/复制链接),读 `generated_files`;office 走下载/外部打开。
- 域名部署持久鉴权(签名 URL / 会话鉴权,替代重启轮换的 loopback token)。

---

## Self-Review

**Spec 覆盖：** FileBaseURL 配置→T1;domain 字段→T2;捕获 write_file→T3;落盘 JSON 列→T4;/v1/files 端点+fileURL→T5;出口(response/turn/历史 拼 url)→T6;全绿+真机→T7。核心不变量(存路径、现拼 URL)在 T4(存)/T5(fileURL)/T6(出口拼) 一致。fail-loud(空=[]、越权 403、缺文件 404、path 缺失报错、非法 base 启动挂、JSON 错包装)贯穿。

**类型一致性：** `GeneratedFiles []string`(T2)在 T3(runtime 填)/T4(storage 存)/T6(recordAssistantTurn) 一致;`GeneratedFile` DTO(T6)由 fileURL(T5)填;`fileURL`/`resolveInWorkspace`(T5)被 T6 用;`workspaceRelPath`(T3)产出的相对路径与 T5 `resolveInWorkspace` 消费的相对路径同形(slash 相对 root)。列顺序 generated_files 接在 token 列后,INSERT/SELECT/scan 三处同步(T4)。

**占位符：** T1/T3/T5/T6 的测试因依赖现有测试设施(config 校验测试、runtime tool-loop、server handler 构造),给 grep 定位+断言契约;schema/domain/端点/helper 等含完整代码。无 TBD。
