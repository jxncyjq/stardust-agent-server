package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// 这条守 P5 Task 3 复审的 🔴 C2 在**产生线上字节的那一层**的一半：
// function.arguments 必须是一个 JSON **对象**的字符串。
//
// 复审用探针挖出来的形状是 `"arguments":"null"`——json.Marshal 一个 nil map 得到的
// 就是 null，而 null 不是对象：Anthropic 兼容网关会把它解成 tool_use.input，过不了
// input schema。这条 Critical 一直没人发现，正是因为当时没有任何测试跑到 adapter，
// 全部停在 port.InferenceMessage 层。
//
// 断言的是 HTTP 请求体里真实的那几个字节，不是某个中间结构体的字段。

// captureOpenAIChatRequestBody 起一个假 provider，把它收到的请求体原样解回来。
func captureOpenAIChatRequestBody(t *testing.T, req port.InferenceRequest) map[string]any {
	t.Helper()
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"好的"}}]}`))
	}))
	t.Cleanup(server.Close)

	client := NewHTTPMaasClient(HTTPMaasConfig{
		BaseURL: server.URL,
		Model:   "deepseek-chat",
		Client:  server.Client(),
	})
	if _, err := client.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if body == nil {
		t.Fatal("假 provider 一次请求都没收到")
	}
	return body
}

// wireToolCallArguments 从请求体里取出第 index 条消息的第一次调用的 arguments 字面量。
func wireToolCallArguments(t *testing.T, body map[string]any, index int) string {
	t.Helper()
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) <= index {
		t.Fatalf("请求体里没有 messages[%d]：%+v", index, body)
	}
	msg, ok := messages[index].(map[string]any)
	if !ok {
		t.Fatalf("messages[%d] 不是对象：%+v", index, messages[index])
	}
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) == 0 {
		t.Fatalf("messages[%d] 没有 tool_calls：%+v", index, msg)
	}
	call, ok := calls[0].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[0] 不是对象：%+v", calls[0])
	}
	fn, ok := call["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[0].function 不是对象：%+v", call)
	}
	args, ok := fn["arguments"].(string)
	if !ok {
		t.Fatalf("function.arguments 不是字符串：%+v", fn)
	}
	return args
}

// assertIsJSONObjectString 断言那段字面量是一个 JSON **对象**的字符串。
// "null" 是合法 JSON，却不是对象——这正是要抓的形状，所以不能只 Unmarshal 进 any。
func assertIsJSONObjectString(t *testing.T, arguments string) map[string]any {
	t.Helper()
	if arguments == "null" {
		t.Fatalf(`function.arguments = "null"：provider 契约要的是一个 JSON 对象串`)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(arguments), &obj); err != nil {
		t.Fatalf("function.arguments = %q，不是合法 JSON：%v", arguments, err)
	}
	if obj == nil {
		t.Fatalf("function.arguments = %q，解出来是 null 而不是对象", arguments)
	}
	return obj
}

// 一条 Arguments 为 nil 的历史调用不得在线上变成 "null"。
func TestToolCallsWithNoArgumentsAreEncodedAsAnEmptyObject(t *testing.T) {
	t.Parallel()

	body := captureOpenAIChatRequestBody(t, port.InferenceRequest{
		RequestID: "r",
		Messages: []port.InferenceMessage{
			{Role: port.RoleUser, Content: "接着说"},
			{Role: port.RoleAssistant, Content: "我读一下", ToolCalls: []domain.ToolCall{
				// Arguments 刻意留 nil：这正是投影修好之前历史调用的形状。
				{ID: "hist-c1", Name: "read_file"},
			}},
			{Role: port.RoleTool, ToolCallID: "hist-c1", Content: "notes 正文"},
		},
	})

	got := wireToolCallArguments(t, body, 1)
	obj := assertIsJSONObjectString(t, got)
	if len(obj) != 0 {
		t.Errorf("function.arguments = %q，要一个空对象", got)
	}
}

// 有参数时原样上线，别被上面那条规则顺手抹平。
func TestToolCallArgumentsReachTheWireVerbatim(t *testing.T) {
	t.Parallel()

	body := captureOpenAIChatRequestBody(t, port.InferenceRequest{
		RequestID: "r",
		Messages: []port.InferenceMessage{
			{Role: port.RoleUser, Content: "接着说"},
			{Role: port.RoleAssistant, ToolCalls: []domain.ToolCall{
				{ID: "hist-c1", Name: "read_file", Arguments: map[string]string{"path": "notes.md"}},
			}},
			{Role: port.RoleTool, ToolCallID: "hist-c1", Content: "notes 正文"},
		},
	})

	obj := assertIsJSONObjectString(t, wireToolCallArguments(t, body, 1))
	if obj["path"] != "notes.md" {
		t.Errorf("function.arguments 里的 path = %v, want notes.md", obj["path"])
	}
}
