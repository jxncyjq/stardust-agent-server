# 多轮 messages 改造实施计划（根治工具循环）

> **For agentic workers:** 用 superpowers:executing-plans 逐任务执行。步骤用 `- [ ]` 复选框跟踪。

**Goal:** 把推理请求从「每轮重发一条拼接好的 user message」改成标准 OpenAI 多轮对话（assistant 带 tool_calls + role=tool 回填结果），让模型能看见自己上一轮调用过什么，消除同参数工具重复调用的死循环。

**Architecture:** `port.InferenceRequest` 增加可选 `Messages []InferenceMessage`（非空时权威，与 `Prompt` 互斥）。`http_maas` 在 Messages 模式下渲染多条消息；`runtime.loopState` 用 append-only 消息数组替代按 dedupKey 覆盖的 `toolCtx`；超预算时折叠最旧 tool 消息的内容（不删消息，保持 assistant/tool 配对）；新增重复调用断路器；checkpoint schema 升 v4 持久化消息数组。

**Tech Stack:** Go 1.26；DeepSeek（OpenAI 兼容 chat completions）；标准库 testing。

## 事故背景（本计划的来由）

任务 `gui-task-1784786476651159000`（「给 hello.txt 加一行」）实测：

| 指标 | 值 |
|---|---|
| read_file 执行次数 | **152** |
| list_files | 11 |
| write_file | 1 |
| 用时 | 554s |
| 累计输入 token | 506.8k（缓存 414.7k） |

证据来自 `agent.db` 的 `audit_events`（时间窗 2026-07-23T06:01:16 ~ 06:11:00）。

根因三段：

1. `http_maas.go:115` 每轮只发 `[{role:"user", content: 拼接串}]`，模型自己上一轮的输出与 tool_call 意图完全不进上下文。
2. `runtime.go mergeToolResults` 按 `dedupKey(name+args)` 覆盖，重复读同一文件 → 同一条目被替换，文本不变。
3. 于是每轮 prompt **逐字节相同**（`composePrompt` 不含轮次号/调用历史）→ 模型重复输出同一个 tool call → 死循环，靠采样随机性才在第 152 次后跳出。

`max_tool_rounds=0`（不限）拆掉了唯一的刹车；旧的 4 轮硬限一直把这个循环掩盖成「轮次不够」。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：不得静默兜底。契约冲突返回 error 并用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装。
- 门禁：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。
- `-race` 在 Windows 跑不了（无 gcc）。用 WSL Ubuntu-22.04，`GOOS=linux GOARCH=amd64 CGO_ENABLED=1 CC=gcc`。
- 公开 API 必须有以标识符名开头的 Go doc 注释。
- 单轮调用方（`internal/cognitive/compressor.go`、`internal/runtime/moa.go`）继续走 `Prompt` 路径，本次不改。

## File Structure

| 文件 | 责任 | 动作 |
|---|---|---|
| `internal/port/ports.go` | `InferenceMessage` 类型 + `InferenceRequest.Messages` 契约 | 修改 |
| `internal/adapter/http_maas.go` | Messages → OpenAI chat 请求体渲染 | 修改 |
| `internal/adapter/maas.go` | `RecordingMaas` 记录 Messages | 修改 |
| `internal/runtime/messages.go` | 消息数组构建、预算折叠、重复调用断路器（新文件，把逻辑从 runtime.go 拆出来） | 创建 |
| `internal/runtime/runtime.go` | loopState 改用消息数组；generate 走 Messages；删 toolCtx 合并 | 修改 |
| `internal/sessionstate/checkpoint.go` | schema v4：`Messages` 快照替代 `ToolEntries` | 修改 |
| `internal/runtime/checkpoint.go` | 快照/还原消息数组 | 修改 |
| `internal/runtime/messages_test.go` | 折叠与断路器单测 | 创建 |
| `internal/runtime/multiturn_test.go` | 端到端：模型看得见自己上一轮 tool_call；循环被断 | 创建 |

---

### Task 1: port 契约 —— InferenceMessage 与互斥校验

**Files:**
- Modify: `internal/port/ports.go`
- Test: `internal/port/ports_test.go`（新建）

**Interfaces:**
- Produces: `port.InferenceMessage{Role, Content, Images, ToolCalls, ToolCallID}`；`port.InferenceRequest.Messages []InferenceMessage`；`func (r InferenceRequest) Validate() error`
- 角色常量：`port.RoleUser = "user"`、`port.RoleAssistant = "assistant"`、`port.RoleTool = "tool"`

- [ ] **Step 1: 写失败测试**

```go
func TestInferenceRequestValidateRejectsBothPromptAndMessages(t *testing.T) {
	req := port.InferenceRequest{
		Prompt:   "hi",
		Messages: []port.InferenceMessage{{Role: port.RoleUser, Content: "hi"}},
	}
	err := req.Validate()
	if err == nil {
		t.Fatal("expected error when both Prompt and Messages are set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error must explain the conflict, got %v", err)
	}
}

func TestInferenceRequestValidateRejectsUnknownRole(t *testing.T) {
	req := port.InferenceRequest{Messages: []port.InferenceMessage{{Role: "system-ish", Content: "x"}}}
	if err := req.Validate(); err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestInferenceRequestValidateRequiresToolCallIDOnToolMessage(t *testing.T) {
	req := port.InferenceRequest{Messages: []port.InferenceMessage{{Role: port.RoleTool, Content: "out"}}}
	if err := req.Validate(); err == nil {
		t.Fatal("expected error for tool message without ToolCallID")
	}
}

func TestInferenceRequestValidateAcceptsPromptOnly(t *testing.T) {
	if err := (port.InferenceRequest{Prompt: "hi"}).Validate(); err != nil {
		t.Fatalf("prompt-only request must stay valid: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/port/ -run TestInferenceRequestValidate -v` → 编译失败（`InferenceMessage` 未定义）。

- [ ] **Step 3: 最小实现**

```go
// Roles accepted in InferenceRequest.Messages. The system role is deliberately
// absent: task framing goes in the first user message, matching the single-turn
// contract this extends.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// InferenceMessage is one turn of a multi-turn exchange. Role decides which
// fields are meaningful: Images only on user, ToolCalls only on assistant,
// ToolCallID only on tool (and required there — an OpenAI-compatible provider
// rejects a tool message it cannot pair with a preceding tool call).
type InferenceMessage struct {
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	Images     []string          `json:"images,omitempty"`
	ToolCalls  []domain.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

// Validate enforces the request's shape before it reaches an adapter. Messages
// and Prompt are mutually exclusive: accepting both would leave which one the
// model actually sees up to each adapter, and a caller that set the wrong one
// would silently get an answer to a different question.
func (r InferenceRequest) Validate() error {
	if len(r.Messages) == 0 {
		return nil
	}
	if strings.TrimSpace(r.Prompt) != "" {
		return fmt.Errorf("inference request %s: Prompt and Messages are mutually exclusive", r.RequestID)
	}
	for i, msg := range r.Messages {
		switch msg.Role {
		case RoleUser, RoleAssistant:
		case RoleTool:
			if strings.TrimSpace(msg.ToolCallID) == "" {
				return fmt.Errorf("inference request %s: message %d has role tool without ToolCallID", r.RequestID, i)
			}
		default:
			return fmt.Errorf("inference request %s: message %d has unknown role %q", r.RequestID, i, msg.Role)
		}
	}
	return nil
}
```

`InferenceRequest` 增字段：

```go
	// Messages carries a multi-turn exchange (user / assistant-with-tool_calls /
	// tool-result). When non-empty it is authoritative and Prompt must be empty:
	// the tool loop uses it so the model can see the calls it already made, which
	// a re-sent single user message cannot express.
	Messages []InferenceMessage
```

- [ ] **Step 4: 跑测试确认通过**

`go test ./internal/port/ -v`

- [ ] **Step 5: 提交**

```bash
git add internal/port/ports.go internal/port/ports_test.go
git commit -m "feat(port): InferenceRequest 支持多轮 messages 契约"
```

---

### Task 2: adapter —— 渲染多轮请求体

**Files:**
- Modify: `internal/adapter/http_maas.go`（`generateOpenAIChat`）
- Modify: `internal/adapter/maas.go`（`RecordingMaas`）
- Test: `internal/adapter/http_maas_test.go`

**Interfaces:**
- Consumes: `port.InferenceMessage`、`port.InferenceRequest.Validate`
- Produces: 请求体中 assistant 消息带 `tool_calls[{id,type:"function",function:{name,arguments}}]`，tool 消息带 `tool_call_id`

- [ ] **Step 1: 写失败测试**

```go
func TestGenerateOpenAIChatSendsMultiTurnMessages(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"done"}}]}`)
	}))
	defer srv.Close()

	client := NewHTTPMaasClient(HTTPMaasConfig{BaseURL: srv.URL, EndpointPath: "/v1/chat/completions", Protocol: "openai_chat"})
	_, err := client.Generate(context.Background(), port.InferenceRequest{
		Messages: []port.InferenceMessage{
			{Role: port.RoleUser, Content: "read hello.txt"},
			{Role: port.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "call-1", Name: "read_file", Arguments: map[string]string{"path": "hello.txt"}}}},
			{Role: port.RoleTool, ToolCallID: "call-1", Content: "hello agent"},
		},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	msgs, _ := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d (%v)", len(msgs), body["messages"])
	}
	assistant, _ := msgs[1].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant message must carry tool_calls, got %v", assistant)
	}
	call, _ := calls[0].(map[string]any)
	if call["id"] != "call-1" {
		t.Fatalf("tool call id must round-trip, got %v", call["id"])
	}
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "read_file" || !strings.Contains(fn["arguments"].(string), "hello.txt") {
		t.Fatalf("tool call function must carry name+arguments, got %v", fn)
	}
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call-1" {
		t.Fatalf("tool result must be a tool message paired by id, got %v", toolMsg)
	}
}

func TestGenerateRejectsPromptAndMessagesTogether(t *testing.T) {
	client := NewHTTPMaasClient(HTTPMaasConfig{BaseURL: "http://unused", Protocol: "openai_chat"})
	_, err := client.Generate(context.Background(), port.InferenceRequest{
		Prompt:   "hi",
		Messages: []port.InferenceMessage{{Role: port.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("adapter must reject a request that sets both")
	}
}
```

（`NewHTTPMaasClient`/`HTTPMaasConfig` 的实际字段名以文件现状为准，测试里按现有构造方式写。）

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/adapter/ -run 'MultiTurn|PromptAndMessages' -v`

- [ ] **Step 3: 最小实现**

`generateOpenAIChat` 开头加校验与分支：

```go
	if err := req.Validate(); err != nil {
		return port.InferenceResponse{}, fmt.Errorf("validate inference request: %w", err)
	}
	messages, err := c.openAIChatMessages(req)
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("build openai chat messages: %w", err)
	}
	body, err := json.Marshal(openAIChatCompletionRequest{
		Model:    c.model,
		Messages: messages,
		Tools:    openAIChatTools(req.Tools),
	})
```

新方法：

```go
// openAIChatMessages renders the request as OpenAI chat messages. With
// req.Messages empty it produces the historical single user message (byte-for-
// byte unchanged, prompt cache breakpoint included); otherwise it renders the
// multi-turn exchange, pairing each tool result with the assistant tool call
// that produced it.
func (c *HTTPMaasClient) openAIChatMessages(req port.InferenceRequest) ([]openAIChatRequestMessage, error) {
	if len(req.Messages) == 0 {
		stablePrefixLen := 0
		if c.enablePromptCache {
			stablePrefixLen = req.StablePrefixLen
		}
		content, err := openAIChatUserContent(req.Prompt, req.Images, stablePrefixLen)
		if err != nil {
			return nil, fmt.Errorf("build openai chat user content: %w", err)
		}
		return []openAIChatRequestMessage{{Role: "user", Content: content}}, nil
	}
	out := make([]openAIChatRequestMessage, 0, len(req.Messages))
	for i, msg := range req.Messages {
		switch msg.Role {
		case port.RoleUser:
			content, err := openAIChatUserContent(msg.Content, msg.Images, 0)
			if err != nil {
				return nil, fmt.Errorf("build user content for message %d: %w", i, err)
			}
			out = append(out, openAIChatRequestMessage{Role: "user", Content: content})
		case port.RoleAssistant:
			calls, err := openAIChatRequestToolCalls(msg.ToolCalls)
			if err != nil {
				return nil, fmt.Errorf("encode tool calls for message %d: %w", i, err)
			}
			out = append(out, openAIChatRequestMessage{Role: "assistant", Content: msg.Content, ToolCalls: calls})
		case port.RoleTool:
			out = append(out, openAIChatRequestMessage{Role: "tool", Content: msg.Content, ToolCallID: msg.ToolCallID})
		default:
			return nil, fmt.Errorf("message %d has unknown role %q", i, msg.Role)
		}
	}
	return out, nil
}

// openAIChatRequestToolCalls re-encodes the domain tool calls the model
// previously emitted. Arguments were decoded into a string map on the way in;
// marshalling them back is lossless for the model's purposes (it sees the same
// keys and values) and is what the provider requires on the way out.
func openAIChatRequestToolCalls(calls []domain.ToolCall) ([]openAIChatToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]openAIChatToolCall, 0, len(calls))
	for _, call := range calls {
		args, err := json.Marshal(call.Arguments)
		if err != nil {
			return nil, fmt.Errorf("marshal arguments of tool call %s: %w", call.ID, err)
		}
		out = append(out, openAIChatToolCall{
			ID:       call.ID,
			Type:     "function",
			Function: openAIChatCallFunction{Name: call.Name, Arguments: string(args)},
		})
	}
	return out, nil
}
```

`openAIChatRequestMessage` 加字段：

```go
type openAIChatRequestMessage struct {
	Role       string               `json:"role"`
	Content    any                  `json:"content,omitempty"`
	ToolCalls  []openAIChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
}
```

`RecordingMaas` 同步记录 `req.Messages`（字段 `LastMessages []port.InferenceMessage`），并同样先 `req.Validate()`。

- [ ] **Step 4: 跑测试确认通过**

`go test ./internal/adapter/ -v`

- [ ] **Step 5: 提交**

```bash
git add internal/adapter/
git commit -m "feat(adapter): openai chat 渲染多轮 messages（assistant tool_calls + tool 结果）"
```

---

### Task 3: runtime —— loopState 改 append-only 消息数组

**Files:**
- Create: `internal/runtime/messages.go`
- Modify: `internal/runtime/runtime.go`
- Test: `internal/runtime/messages_test.go`

**Interfaces:**
- Produces:
  - `type conversation struct { messages []port.InferenceMessage }`
  - `func newConversation(basePrompt string, images []string) *conversation`
  - `func (c *conversation) appendAssistant(text string, calls []domain.ToolCall)`
  - `func (c *conversation) appendToolResults(calls []domain.ToolCall, results []domain.ToolResult, maxResultChars int)`
  - `func (c *conversation) appendUser(text string)`
  - `func (c *conversation) render(maxChars int) []port.InferenceMessage`
- Consumes: Task 1 的 `port.InferenceMessage`

- [ ] **Step 1: 写失败测试**

```go
func TestConversationRecordsAssistantToolCallsThenResults(t *testing.T) {
	c := newConversation("do the thing", nil)
	calls := []domain.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "a.txt"}}}
	c.appendAssistant("", calls)
	c.appendToolResults(calls, []domain.ToolResult{{CallID: "c1", Success: true, Output: "content"}}, 0)

	msgs := c.render(0)
	if len(msgs) != 3 {
		t.Fatalf("want user+assistant+tool, got %d: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != port.RoleAssistant || len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("assistant turn must carry the calls the model made: %+v", msgs[1])
	}
	if msgs[2].Role != port.RoleTool || msgs[2].ToolCallID != "c1" || msgs[2].Content != "content" {
		t.Fatalf("tool result must be paired to its call: %+v", msgs[2])
	}
}

// The 152-read incident: the same call repeated must stay visible as separate
// turns. Collapsing them (the old dedup-by-args behaviour) is exactly what made
// every round look identical to the model.
func TestConversationKeepsRepeatedIdenticalCallsAsDistinctTurns(t *testing.T) {
	c := newConversation("base", nil)
	for i := range 3 {
		calls := []domain.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read_file", Arguments: map[string]string{"path": "a.txt"}}}
		c.appendAssistant("", calls)
		c.appendToolResults(calls, []domain.ToolResult{{CallID: calls[0].ID, Success: true, Output: "same"}}, 0)
	}
	msgs := c.render(0)
	if len(msgs) != 7 {
		t.Fatalf("3 rounds must produce 1 user + 3 assistant + 3 tool = 7 messages, got %d", len(msgs))
	}
}

func TestConversationTruncatesSingleToolResult(t *testing.T) {
	c := newConversation("base", nil)
	calls := []domain.ToolCall{{ID: "c1", Name: "read_file"}}
	c.appendAssistant("", calls)
	c.appendToolResults(calls, []domain.ToolResult{{CallID: "c1", Success: true, Output: strings.Repeat("x", 100)}}, 10)

	msgs := c.render(0)
	if !strings.Contains(msgs[2].Content, "truncated") {
		t.Fatalf("oversized tool output must be truncated with a marker: %q", msgs[2].Content)
	}
}

func TestConversationRecordsFailedToolAsToolMessage(t *testing.T) {
	c := newConversation("base", nil)
	calls := []domain.ToolCall{{ID: "c1", Name: "read_file"}}
	c.appendAssistant("", calls)
	c.appendToolResults(calls, []domain.ToolResult{{CallID: "c1", Success: false, Error: "no such file"}}, 0)

	msgs := c.render(0)
	if msgs[2].Role != port.RoleTool || !strings.Contains(msgs[2].Content, "no such file") {
		t.Fatalf("tool failure must reach the model as its tool message: %+v", msgs[2])
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/runtime/ -run TestConversation -v` → 编译失败。

- [ ] **Step 3: 最小实现（`internal/runtime/messages.go`）**

```go
// conversation accumulates the multi-turn exchange of one tool loop. It is
// append-only by design: the model must be able to see that it already made a
// call, which is precisely what the previous dedup-by-(name,args) context could
// not express — repeated identical calls collapsed into one entry, every round's
// prompt came out byte-identical, and the model kept re-issuing the same call.
type conversation struct {
	messages []port.InferenceMessage
}

// newConversation starts an exchange with the task framing as the first user
// message. images ride on that message, matching the single-turn contract.
func newConversation(basePrompt string, images []string) *conversation {
	return &conversation{messages: []port.InferenceMessage{{
		Role:    port.RoleUser,
		Content: basePrompt,
		Images:  images,
	}}}
}

// appendAssistant records the model's turn. calls may be empty (a plain textual
// answer); text may be empty (a pure tool-call turn).
func (c *conversation) appendAssistant(text string, calls []domain.ToolCall) {
	c.messages = append(c.messages, port.InferenceMessage{
		Role:      port.RoleAssistant,
		Content:   text,
		ToolCalls: calls,
	})
}

// appendToolResults records one tool message per executed call, paired by call
// ID. A failed call is reported to the model as its own tool message rather than
// being dropped: the model has to see the failure to recover from it.
func (c *conversation) appendToolResults(calls []domain.ToolCall, results []domain.ToolResult, maxResultChars int) {
	byID := make(map[string]domain.ToolResult, len(results))
	for _, res := range results {
		byID[res.CallID] = res
	}
	for _, call := range calls {
		res, ok := byID[call.ID]
		if !ok {
			continue
		}
		content := res.Output
		if !res.Success {
			content = "failed: " + res.Error
		}
		c.messages = append(c.messages, port.InferenceMessage{
			Role:       port.RoleTool,
			ToolCallID: call.ID,
			Content:    truncateText(content, maxResultChars),
		})
	}
}

// appendUser adds an out-of-band instruction turn (loaded capabilities, a
// repeat warning, the final answer-now nudge).
func (c *conversation) appendUser(text string) {
	c.messages = append(c.messages, port.InferenceMessage{Role: port.RoleUser, Content: text})
}
```

`render` 在 Task 4 补预算折叠，本任务先直返：

```go
// render returns the messages to send. maxChars <= 0 disables budget folding.
func (c *conversation) render(maxChars int) []port.InferenceMessage {
	return slices.Clone(c.messages)
}
```

`runtime.go` 改动：
- `loopState` 用 `convo *conversation` 替换 `toolCtx []toolEntry`。
- `RunTask` 首轮：先建 `convo := newConversation(basePrompt, task.Images)`，再 `r.generateMessages(ctx, requestID, convo, effTools)`。
- `runToolLoop` 每轮：`st.convo.appendAssistant(st.resp.Text, st.resp.ToolCalls)` →执行工具→ `st.convo.appendToolResults(...)` → loaded 非空则 `appendUser(renderLoaded(st.loaded))` → 再 generate。
- 新增：

```go
func (r *Runtime) generateMessages(ctx context.Context, requestID string, convo *conversation, tools *tool.Registry) (port.InferenceResponse, error) {
	return r.maas.Generate(ctx, port.InferenceRequest{
		RequestID: requestID,
		Messages:  convo.render(r.maxPromptChars),
		Tools:     r.inferenceTools(tools),
	})
}
```

- 删除 `mergeToolResults`、`renderToolEntries`、`composePrompt` 的 toolCtx 分支、`toolEntry`、`dedupKey` 中仅服务于合并的用法（`dedupKey` 保留给 Task 5 的断路器）。
- 轮次预算耗尽分支改为 `st.convo.appendUser("[系统] 工具调用已达上限。…")` 后调用 `generateMessages` 且 tools 传 nil。

- [ ] **Step 4: 跑测试确认通过**

`go test ./internal/runtime/ -run TestConversation -v`，随后 `go test ./...`（既有测试会因签名变动失败，本步一并修到全绿）。

- [ ] **Step 5: 提交**

```bash
git add internal/runtime/
git commit -m "feat(runtime): 工具循环改 append-only 多轮消息，删按参数去重的历史合并"
```

---

### Task 4: 预算折叠 —— 不删消息，只折叠最旧 tool 内容

**Files:**
- Modify: `internal/runtime/messages.go`
- Test: `internal/runtime/messages_test.go`

**Interfaces:**
- Consumes: Task 3 的 `conversation`
- Produces: `render(maxChars)` 在超预算时把最旧的 tool 消息内容替换为 `[older tool output trimmed: N chars]`

- [ ] **Step 1: 写失败测试**

```go
func TestRenderFoldsOldestToolOutputWhenOverBudget(t *testing.T) {
	c := newConversation("base", nil)
	for i := range 5 {
		calls := []domain.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read_file"}}
		c.appendAssistant("", calls)
		c.appendToolResults(calls, []domain.ToolResult{{CallID: calls[0].ID, Success: true, Output: strings.Repeat("x", 1000)}}, 0)
	}

	msgs := c.render(2000)

	// Message count is preserved: an OpenAI-compatible provider rejects a tool
	// message whose assistant tool_call is missing, so folding may only rewrite
	// content, never drop turns.
	if len(msgs) != 11 {
		t.Fatalf("folding must preserve the turn structure, got %d messages", len(msgs))
	}
	if !strings.Contains(msgs[2].Content, "trimmed") {
		t.Fatalf("oldest tool output must be folded first: %q", msgs[2].Content)
	}
	if msgs[len(msgs)-1].Content != strings.Repeat("x", 1000) {
		t.Fatal("newest tool output must survive folding intact")
	}
	if total := totalChars(msgs); total > 2000 {
		t.Fatalf("rendered messages must respect the budget, got %d chars", total)
	}
}

func TestRenderNeverFoldsTheFirstUserMessage(t *testing.T) {
	c := newConversation(strings.Repeat("b", 3000), nil)
	msgs := c.render(1000)
	if msgs[0].Content != strings.Repeat("b", 3000) {
		t.Fatal("task framing is pinned: folding it would drop the instructions")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/runtime/ -run TestRender -v`

- [ ] **Step 3: 最小实现**

```go
// render returns the messages to send, folding the oldest tool outputs first
// when the exchange exceeds maxChars. It never drops a message: an
// OpenAI-compatible provider rejects a tool message whose assistant tool_call
// is absent, so the turn structure is load-bearing. The first user message
// (task framing) is pinned — trimming it would silently delete the instructions
// the run is being judged against. maxChars <= 0 disables folding.
func (c *conversation) render(maxChars int) []port.InferenceMessage {
	out := slices.Clone(c.messages)
	if maxChars <= 0 || totalChars(out) <= maxChars {
		return out
	}
	for i := range out {
		if out[i].Role != port.RoleTool {
			continue
		}
		dropped := len([]rune(out[i].Content))
		if dropped == 0 {
			continue
		}
		out[i].Content = fmt.Sprintf("[older tool output trimmed: %d chars]", dropped)
		if totalChars(out) <= maxChars {
			break
		}
	}
	return out
}

// totalChars is the rune length of every message's content, the budget unit
// used by render.
func totalChars(msgs []port.InferenceMessage) int {
	total := 0
	for _, msg := range msgs {
		total += len([]rune(msg.Content))
	}
	return total
}
```

- [ ] **Step 4: 跑测试确认通过**

`go test ./internal/runtime/ -run 'TestRender|TestConversation' -v`

- [ ] **Step 5: 提交**

```bash
git add internal/runtime/messages.go internal/runtime/messages_test.go
git commit -m "feat(runtime): 多轮历史超预算时折叠最旧 tool 输出，保住轮次结构"
```

---

### Task 5: 重复调用断路器

**Files:**
- Modify: `internal/runtime/messages.go`（计数与判定）
- Modify: `internal/runtime/runtime.go`（接线 + 事件）
- Test: `internal/runtime/messages_test.go`

**Interfaces:**
- Produces:
  - `func repeatedCallStreak(msgs []port.InferenceMessage, calls []domain.ToolCall) int`
  - 常量 `repeatWarnStreak = 3`、`repeatAbortStreak = 8`
- 行为：连续第 3 次起，工具结果之后追加一条 user 警告消息；第 8 次直接停止工具循环，走无工具收尾，并发 `tool_loop_broken` 事件。

- [ ] **Step 1: 写失败测试**

```go
func TestRepeatedCallStreakCountsIdenticalConsecutiveCalls(t *testing.T) {
	c := newConversation("base", nil)
	same := []domain.ToolCall{{Name: "read_file", Arguments: map[string]string{"path": "a.txt"}}}
	for i := range 3 {
		calls := []domain.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read_file", Arguments: map[string]string{"path": "a.txt"}}}
		c.appendAssistant("", calls)
		c.appendToolResults(calls, []domain.ToolResult{{CallID: calls[0].ID, Success: true, Output: "same"}}, 0)
	}
	if got := repeatedCallStreak(c.messages, same); got != 3 {
		t.Fatalf("want streak 3, got %d", got)
	}
}

func TestRepeatedCallStreakResetsOnDifferentArguments(t *testing.T) {
	c := newConversation("base", nil)
	for _, path := range []string{"a.txt", "a.txt", "b.txt"} {
		calls := []domain.ToolCall{{ID: "c-" + path, Name: "read_file", Arguments: map[string]string{"path": path}}}
		c.appendAssistant("", calls)
		c.appendToolResults(calls, []domain.ToolResult{{CallID: calls[0].ID, Success: true, Output: "x"}}, 0)
	}
	next := []domain.ToolCall{{Name: "read_file", Arguments: map[string]string{"path": "b.txt"}}}
	if got := repeatedCallStreak(c.messages, next); got != 1 {
		t.Fatalf("a different argument set must reset the streak, got %d", got)
	}
}
```

runtime 层行为测试（`internal/runtime/multiturn_test.go`，与 Task 7 同文件）：

```go
// A model stuck re-reading the same file must be stopped, not left to burn 152
// rounds as it did on 2026-07-23.
func TestRuntimeBreaksRepeatedIdenticalToolCallLoop(t *testing.T) {
	maas := &loopingMaas{call: domain.ToolCall{Name: "read_file", Arguments: map[string]string{"path": "hello.txt"}}}
	rt := NewRuntime(Config{Maas: maas, Tools: testRegistryWithReadFile(t), MaxToolRounds: 0 /* unlimited */})

	run, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read it"})
	if err != nil {
		t.Fatalf("run task: %v", err)
	}
	if maas.calls > repeatAbortStreak+2 {
		t.Fatalf("loop must be cut near the abort streak, model was called %d times", maas.calls)
	}
	if run.Result == "" {
		t.Fatal("a broken loop must still produce an answer for the user")
	}
	if !maas.sawWarning {
		t.Fatal("the model must be told it is repeating before the loop is cut")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/runtime/ -run 'TestRepeatedCallStreak|TestRuntimeBreaksRepeated' -v`

- [ ] **Step 3: 最小实现**

```go
const (
	// repeatWarnStreak is how many consecutive identical tool calls (same name
	// and arguments) trigger an explicit warning turn, and repeatAbortStreak how
	// many end the loop. A model that cannot see progress in its own history
	// will happily repeat a call forever — on 2026-07-23 one task read the same
	// file 152 times over 554s. Warning first keeps a legitimately repeated call
	// (polling a file that is expected to change) workable.
	repeatWarnStreak  = 3
	repeatAbortStreak = 8
)

// repeatedCallStreak reports how many consecutive assistant turns — counting the
// pending calls as the newest — requested exactly the same tool calls. It
// returns 1 when the pending calls differ from the previous turn.
func repeatedCallStreak(msgs []port.InferenceMessage, calls []domain.ToolCall) int {
	if len(calls) == 0 {
		return 0
	}
	want := callsKey(calls)
	streak := 1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != port.RoleAssistant || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		if callsKey(msgs[i].ToolCalls) != want {
			break
		}
		streak++
	}
	return streak
}

// callsKey identifies a whole round's tool calls by name+arguments, ignoring
// call IDs (which are fresh every round and would defeat the comparison).
func callsKey(calls []domain.ToolCall) string {
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, dedupKey(call))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}
```

`runToolLoop` 内接线（执行工具并追加结果之后、下一次 generate 之前）：

```go
		streak := repeatedCallStreak(st.convo.messages, st.resp.ToolCalls)
		if streak >= repeatAbortStreak {
			if err := r.events.Publish(ctx, domain.RuntimeEvent{
				Type:      "tool_loop_broken",
				TaskID:    task.ID,
				Message:   fmt.Sprintf("同一工具调用连续重复 %d 次，已停止工具循环", streak),
				CreatedAt: time.Now(),
			}); err != nil {
				return domain.TaskRun{}, fmt.Errorf("publish tool loop broken event: %w", err)
			}
			r.logger.Warn("tool loop broken: identical call repeated",
				"task_id", task.ID, "streak", streak, "calls", callsKey(st.resp.ToolCalls))
			break
		}
		if streak >= repeatWarnStreak {
			st.convo.appendUser(fmt.Sprintf(
				"[系统] 你已连续 %d 次以完全相同的参数调用同一工具，结果没有变化。不要再重复该调用：改用其他工具，或基于已有信息直接给出最终回答。", streak))
		}
```

`break` 落到既有的「工具预算耗尽」收尾分支（`len(st.resp.ToolCalls) > 0` 仍为真 → 走 no-tools 收尾），用户拿到答案而不是「任务执行失败」。

- [ ] **Step 4: 跑测试确认通过**

`go test ./internal/runtime/ -v`

- [ ] **Step 5: 提交**

```bash
git add internal/runtime/
git commit -m "feat(runtime): 重复工具调用断路器（3 次警告 / 8 次断流并收尾）"
```

---

### Task 6: checkpoint schema v4 —— 持久化消息数组

**Files:**
- Modify: `internal/sessionstate/checkpoint.go`
- Modify: `internal/runtime/checkpoint.go`
- Test: `internal/sessionstate/checkpoint_test.go`、`internal/runtime/checkpoint_test.go`

**Interfaces:**
- Produces: `sessionstate.MessageSnapshot{Role, Content, Images, ToolCalls, ToolCallID}`；`Checkpoint.Messages []MessageSnapshot`；`CheckpointSchemaVersion = 4`
- 移除：`Checkpoint.ToolEntries`、`ToolEntrySnapshot`（连同 `restoreToolEntries`/`snapshotToolEntries`）

- [ ] **Step 1: 写失败测试**

```go
func TestCheckpointRoundTripsMessages(t *testing.T) {
	dir := t.TempDir()
	store := sessionstate.NewStore(dir)
	cp := sessionstate.Checkpoint{
		SchemaVersion: sessionstate.CheckpointSchemaVersion,
		TaskID:        "t1",
		SessionKey:    "s1",
		Messages: []sessionstate.MessageSnapshot{
			{Role: "user", Content: "base"},
			{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "read_file"}}},
			{Role: "tool", ToolCallID: "c1", Content: "out"},
		},
	}
	if err := store.Save(cp, ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := store.Load("s1", "")
	if err != nil || !ok {
		t.Fatalf("load: %v ok=%v", err, ok)
	}
	if len(got.Messages) != 3 || got.Messages[2].ToolCallID != "c1" {
		t.Fatalf("messages must round-trip intact: %+v", got.Messages)
	}
}

func TestLoadRejectsPreviousSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	// 写一个 v3 checkpoint 文件（旧 tool_entries 布局）后 Load 必须 fail-loud。
	// 具体写法沿用本文件既有的 writeRawCheckpoint 辅助。
}
```

（`NewStore`/`Save`/`Load` 的实际签名以文件现状为准。）

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/sessionstate/ -run TestCheckpoint -v`

- [ ] **Step 3: 最小实现**

```go
// CheckpointSchemaVersion versions the on-disk checkpoint format.
//
// v4 replaces ToolEntries (the deduplicated rendered tool context of the
// single-user-message era) with Messages, the append-only multi-turn exchange.
// A v3 checkpoint cannot be upgraded: its tool context was collapsed by
// (name, arguments) and the assistant turns were never recorded, so the
// conversation it describes cannot be reconstructed. Load rejects it outright
// rather than resuming a run from a history that never existed.
const CheckpointSchemaVersion = 4

// MessageSnapshot is the serialisable form of one conversation turn.
type MessageSnapshot struct {
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	Images     []string          `json:"images,omitempty"`
	ToolCalls  []domain.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}
```

`Checkpoint`：删 `ToolEntries`，加 `Messages []MessageSnapshot json:"messages"`。

`internal/runtime/checkpoint.go`：`snapshotMessages(convo)` / `restoreConversation(cp.Messages)` 替换 `snapshotToolEntries`/`restoreToolEntries`；`RunTask` 的 resume 分支用 `convo: restoreConversation(cp.Messages)`。

- [ ] **Step 4: 跑测试确认通过**

`go test ./internal/sessionstate/ ./internal/runtime/ -v`

- [ ] **Step 5: 提交**

```bash
git add internal/sessionstate/ internal/runtime/
git commit -m "feat(sessionstate): checkpoint v4 持久化多轮消息，拒绝无法升级的 v3"
```

---

### Task 7: 端到端回归 —— 模型看得见自己的调用

**Files:**
- Create/Modify: `internal/runtime/multiturn_test.go`
- Test: 同文件

**Interfaces:**
- Consumes: Task 1–6 全部

- [ ] **Step 1: 写失败测试**

```go
// The regression this whole change exists for: on the second round the model
// must receive its own previous tool call and the result, as separate turns.
func TestSecondRoundCarriesPriorAssistantCallAndToolResult(t *testing.T) {
	maas := &recordingRoundsMaas{
		responses: []port.InferenceResponse{
			{ToolCalls: []domain.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "hello.txt"}}}},
			{Text: "done"},
		},
	}
	rt := NewRuntime(Config{Maas: maas, Tools: testRegistryWithReadFile(t)})
	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read hello.txt"}); err != nil {
		t.Fatalf("run task: %v", err)
	}

	second := maas.requests[1]
	if len(second.Messages) < 3 {
		t.Fatalf("second round must be multi-turn, got %+v", second.Messages)
	}
	if second.Prompt != "" {
		t.Fatal("multi-turn requests must not also set Prompt")
	}
	var sawAssistantCall, sawToolResult bool
	for _, msg := range second.Messages {
		if msg.Role == port.RoleAssistant && len(msg.ToolCalls) == 1 && msg.ToolCalls[0].Name == "read_file" {
			sawAssistantCall = true
		}
		if msg.Role == port.RoleTool && msg.ToolCallID == "c1" {
			sawToolResult = true
		}
	}
	if !sawAssistantCall || !sawToolResult {
		t.Fatalf("model must see its own call and its result: %+v", second.Messages)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/runtime/ -run TestSecondRoundCarries -v`

- [ ] **Step 3: 实现**

前序任务已覆盖；本步只补测试所需的 fake（`recordingRoundsMaas`、`loopingMaas`、`testRegistryWithReadFile`）。

- [ ] **Step 4: 全量门禁**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l .
```

WSL race：

```bash
wsl -d Ubuntu-22.04 -- bash -lc 'cd /mnt/f/source/stardust/Legion/legion/legionAgent && GOOS=linux GOARCH=amd64 CGO_ENABLED=1 CC=gcc PATH=$HOME/sdk/go/bin:$PATH go test -race ./internal/runtime/ ./internal/adapter/'
```

- [ ] **Step 5: 提交并开 PR**

```bash
git add internal/runtime/
git commit -m "test(runtime): 钉住多轮上下文回归（模型看得见自己上一轮调用）"
```

---

## 人工验证（合并前）

1. `cd legionAgentGUI && run.bat build` 重编（serve 编译进 exe）。
2. 交互启动 GUI，工作目录指向 `F:\source\stardust\Legion\test`，发同一句「为当前目录下的 hello.txt 增加一行 …」。
3. 判据：Audit 面板里 `read_file` 次数应为个位数（对照事故的 152 次）；用时与输入 token 同步大跌；若模型仍重复，应看到 `tool_loop_broken` 事件而不是无限跑。

## 已知取舍

- v3 checkpoint 不可升级，升级后挂起中的任务需重跑（Load 直接 fail-loud 拒绝）。当前无长期挂起任务，代价可接受。
- `domain.ToolCall.Arguments` 是 `map[string]string`，回放时重新 marshal：嵌套 JSON 参数会以字符串形式回放。模型看到的键值语义不变，但与它原始发出的 arguments 不是字节级一致。
- `http_maas.go:370` 的 `json.Unmarshal(...); err == nil {}` 静默吞掉参数解析失败，是既有 fail-loud 违规，不在本计划范围，另案处理。
