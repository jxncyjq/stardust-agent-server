package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// 这一组守 P5 Task 3 复审的 🔴 C1 在 runtime 侧的那一处接线。
//
// 复审的变异实证：把 agent_resolver.go 里那行 `HistoryTranscript: history.Transcript,`
// 删掉，`go test ./...` 全绿。原来的三条 wiring 测试自己调 SessionHistoryForTask、
// 再手工把两个字段填进 Config，测的是「选路选对了形状」+「Runtime 收到会用」，独独
// 没测「生产代码把前者的输出交给了后者」——而那正是这个功能唯一起作用的地方，也正是
// 本仓栽过两次的形状（插件工具、审批仲裁者：接缝在，但那条路上没人调用它）。
//
// 这里断言的是**装配的结果**：走真正的生产入口 ResolveTaskRunner，跑出来的任务里
// 模型到底看见了什么，照 TestEveryBrowserConfigKeyReachesTheRuntime 的范式。

// transcriptOnlyLister 只服务 G3 打开那条路。
//
// ListConversationTurns 报错而不是返回空：这个 double 存在的意义就是把「transcript
// 真的被取了并接上了」与「悄悄退回 turns 那条路」分开。返回 (nil, nil) 会让后者看起来
// 像「这条会话本来就没历史」（CLAUDE.md §0）。
type transcriptOnlyLister struct {
	transcript []port.InferenceMessage
	gotLimit   int
	gotSession string
}

func (l *transcriptOnlyLister) ListConversationTurns(_ context.Context, sessionID string, _ int) ([]domain.ConversationTurn, error) {
	return nil, fmt.Errorf("transcriptOnlyLister: session %q asked for turns, but this double only serves the G3-on transcript path", sessionID)
}

func (l *transcriptOnlyLister) ListConversationTranscript(_ context.Context, sessionID string, limit int) ([]port.InferenceMessage, error) {
	l.gotSession = sessionID
	l.gotLimit = limit
	return append([]port.InferenceMessage(nil), l.transcript...), nil
}

// historyWithOneToolRound 是一段带工具往返的最小历史——工具往返正是 G3 的全部意义，
// 一段没有工具调用的历史在开与关两种设置下投影成同一个形状，什么也证明不了。
func historyWithOneToolRound() []port.InferenceMessage {
	return []port.InferenceMessage{
		{Role: port.RoleUser, Content: historyQuestion + " 读一下 notes.md"},
		{Role: port.RoleAssistant, Content: "我读一下", ToolCalls: []domain.ToolCall{
			{ID: historyCallID, Name: "read_file", Arguments: map[string]string{"path": "notes.md"}},
		}},
		{Role: port.RoleTool, ToolCallID: historyCallID, Content: historyToolOutput},
		{Role: port.RoleAssistant, Content: historyAnswer + " 读完了"},
	}
}

// assertModelSawTheToolRoundTrip 是三个调用点共用的判据：模型收到的那次请求里，历史
// 的工具往返真的在，且每条 tool 消息都配得上前面宣告它的 assistant（配不上 provider
// 会拒收整个请求）。
func assertModelSawTheToolRoundTrip(t *testing.T, msgs []port.InferenceMessage, callSite string) {
	t.Helper()
	sawTool := false
	for _, m := range msgs {
		if m.Role == port.RoleTool && strings.Contains(m.Content, historyToolOutput) {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatalf("%s：开关打开，模型却一条历史 tool 消息都没收到——这条路没把 transcript 接上，"+
			"而它不会报任何错\n%+v", callSite, msgs)
	}
	assertTranscriptIsValid(t, msgs)
}

// TestTheResolverSendsHistoryAsATranscript 守生产接线点②：
// AgentRuntimeResolver.ResolveTaskRunner 里那行 `HistoryTranscript: history.Transcript,`。
//
// 走真入口：构造 resolver → ResolveTaskRunner → 跑它返回的 runner，断言模型看到的
// messages。删掉那一行，这条测试红。
func TestTheResolverSendsHistoryAsATranscript(t *testing.T) {
	t.Parallel()

	maas := &recordingMaas{}
	lister := &transcriptOnlyLister{transcript: historyWithOneToolRound()}
	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Gate: taskgate.NewTaskGate(),
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {ID: "agent-researcher", Role: "researcher", MaasProfile: "deep"},
		}),
		RootConfig: config.Config{
			Runtime: config.RuntimeConfig{MaxToolRounds: 1},
			Session: config.SessionConfig{
				DefaultRecentTurns:    6,
				MaxTurnChars:          6000,
				ToolTranscriptEnabled: true,
			},
		},
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
		MaasFactory: func(string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: maas}, nil
		},
		ConversationTurns: lister,
	})

	task := domain.Task{ID: "task-9", SessionID: "s1", AgentID: "researcher", Input: "接着说"}
	agent, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), task)
	if err != nil {
		t.Fatalf("ResolveTaskRunner error = %v, want nil", err)
	}
	if !ok {
		t.Fatal("ResolveTaskRunner ok = false, want true")
	}
	if _, err := runner.RunTask(context.Background(), agent, task); err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}

	if lister.gotSession != "s1" {
		t.Fatalf("lister 收到的 sessionID = %q, want s1：历史根本没被取", lister.gotSession)
	}
	assertModelSawTheToolRoundTrip(t, maas.first(t).Messages,
		"AgentRuntimeResolver.ResolveTaskRunner")
}

// TestTheTranscriptPathHonoursMaxTurnChars 守复审的 🟠 I2。
//
// session.max_turn_chars 是用户配置的历史体积上限。关闭那条路对每条 turn 做
// truncateText；打开那条路原样透传投影出来的正文，而 user/assistant 事件的文档明写
// 「存全文，不截断」（上限是 P1 的 64 KiB/事件）。于是打开 G3 就等于把用户设的上限
// 悄悄取消——而 G3 的全部代价就是体积（spec §3）。
//
// 这条测试把上限设成一个很小的值，断言送到模型的历史正文真的被截住了。
func TestTheTranscriptPathHonoursMaxTurnChars(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("史", 500)
	lister := &transcriptOnlyLister{transcript: []port.InferenceMessage{
		{Role: port.RoleUser, Content: long},
	}}

	msgs, err := SessionTranscript(context.Background(), lister, config.SessionConfig{
		DefaultRecentTurns: 6,
		MaxTurnChars:       10,
	}, "s1")
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("投影出 %d 条消息，要 1 条", len(msgs))
	}
	if !strings.HasPrefix(msgs[0].Content, strings.Repeat("史", 10)) {
		t.Fatalf("正文开头不是原文的前 10 个字符：%q", msgs[0].Content)
	}
	if strings.Contains(msgs[0].Content, long) {
		t.Error("配置的 session.max_turn_chars 被绕过了：整段正文原样进了模型")
	}
	if !strings.Contains(msgs[0].Content, "OUTPUT HARD-TRUNCATED") {
		t.Errorf("截断没有留痕，模型不知道自己看到的是半截：%q", msgs[0].Content)
	}
}

// TestTheTranscriptPathAsksForABoundedWindow 钉住「打开 G3 不等于把整条会话读回来」：
// 条数上限仍由 DefaultRecentTurns 换算而来。
func TestTheTranscriptPathAsksForABoundedWindow(t *testing.T) {
	t.Parallel()

	lister := &transcriptOnlyLister{transcript: historyWithOneToolRound()}
	if _, err := SessionTranscript(context.Background(), lister, config.SessionConfig{
		DefaultRecentTurns: 6,
	}, "s1"); err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	if want := 6 * transcriptMessagesPerTurn; lister.gotLimit != want {
		t.Errorf("向 store 要了 limit=%d，want %d：条数预算没按 DefaultRecentTurns 换算",
			lister.gotLimit, want)
	}
}

// TestHistoryToolCallsCarryTheirArgumentsOnTheWire 守复审的 🔴 C2，且是**第一条跑到
// adapter 层**的测试——这条 Critical 一直没被发现，根本原因就是所有测试都停在
// port.InferenceMessage 层，没有一条看过线上字节。
//
// 走的是真链路：真 SQLite 事件日志 → 真投影（G3 打开）→ 真 Runtime 组装 → 真 adapter
// 编码 → 假 provider 收到的请求体。断言 function.arguments 是一个合法 JSON **对象**串，
// 不是复审探针挖出来的 "null"（null 是合法 JSON，却不是对象；Anthropic 兼容网关会把它
// 解成 tool_use.input，过不了 input schema）。
func TestHistoryToolCallsCarryTheirArgumentsOnTheWire(t *testing.T) {
	t.Parallel()

	msgs := runTaskWithHistory(t, true)

	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"好的"}}]}`))
	}))
	t.Cleanup(server.Close)
	client := adapter.NewHTTPMaasClient(adapter.HTTPMaasConfig{
		BaseURL: server.URL,
		Model:   "deepseek-chat",
		Client:  server.Client(),
	})
	if _, err := client.Generate(context.Background(), port.InferenceRequest{
		RequestID: "r", Messages: msgs,
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	arguments := onlyWireToolCallArguments(t, body)
	if arguments == "null" {
		t.Fatalf(`历史调用的 function.arguments = "null"：契约要的是一个 JSON 对象串，` +
			`模型也看不见自己当时传了什么参数`)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
		t.Fatalf("function.arguments = %q，不是合法 JSON：%v", arguments, err)
	}
	if decoded == nil {
		t.Fatalf("function.arguments = %q，解出来是 null 而不是对象", arguments)
	}
	// 历史里那次调用传的就是 notes.md（见 seedHistorySession 的 tool/call 事件）。
	if decoded["path"] != "notes.md" {
		t.Errorf("function.arguments 里的 path = %v, want notes.md：参数没从 tool/call 事件取回来",
			decoded["path"])
	}
}

// onlyWireToolCallArguments 从请求体里取出唯一那次工具调用的 arguments 字面量。
func onlyWireToolCallArguments(t *testing.T, body map[string]any) string {
	t.Helper()
	if body == nil {
		t.Fatal("假 provider 一次请求都没收到")
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("请求体里没有 messages：%+v", body)
	}
	var found []string
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		calls, ok := msg["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := call["function"].(map[string]any)
			if !ok {
				t.Fatalf("tool_calls 里没有 function：%+v", call)
			}
			args, ok := fn["arguments"].(string)
			if !ok {
				t.Fatalf("function.arguments 不是字符串：%+v", fn)
			}
			found = append(found, args)
		}
	}
	if len(found) != 1 {
		t.Fatalf("线上有 %d 次工具调用，要 1 次：%+v", len(found), messages)
	}
	return found[0]
}
