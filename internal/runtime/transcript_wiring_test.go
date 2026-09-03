package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/storage"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// historyQuestion 等标记是预置历史带进它那两种形状里的字符串，用来把「历史真的到了
// 模型面前」与「有个长得像的东西到了」区分开。
const (
	historyQuestion   = "HISTORY-QUESTION"
	historyToolOutput = "HISTORY-TOOL-OUTPUT"
	historyAnswer     = "HISTORY-ANSWER"
	historyCallID     = "hist-c1"
	historySessionID  = "sess-hist"
)

// recordingMaas 每次都回同一句纯文本，并把被看到的每个请求留下来。
//
// 它按请求的**内容**作答，绝不按被调用的次数：它一次工具都不要，所以没有可变的东西
// 需要按次数分支——而按次数作答的假模型会让「这次运行发出的请求数与被测路径不同」
// 也照样看起来像被测路径（本包 toolThenAnswerMaas 的注释记着这个教训）。
type recordingMaas struct {
	mu       sync.Mutex
	requests []port.InferenceRequest
}

func (m *recordingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	return port.InferenceResponse{Text: "好的"}, nil
}

// first 是模型在这次任务的**第一次**请求里看到的交流——G3 接线组装出来的那一次。
func (m *recordingMaas) first(t *testing.T) port.InferenceRequest {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		t.Fatal("假模型一次都没被调用：这次任务根本没发出模型请求")
	}
	return m.requests[0]
}

// seedHistorySession 往 sessionID 的事件日志里写一个**已完成的上一个任务**：问一句、
// 一轮调用 read_file 的 assistant、那次调用的结果、收尾的回答。
//
// 这是「带工具往返」的最小历史，而工具往返正是 G3 的全部意义——一段没有工具调用的
// 历史在开与关两种设置下投影成同一个形状，什么也证明不了。
func seedHistorySession(t *testing.T, ctx context.Context, repo *storage.SQLiteRepository, sessionID string) {
	t.Helper()
	at := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	events := []domain.SessionEvent{
		projectionEvent(t, 0, domain.SessionEventTurnStart, at, map[string]any{"turn": 0}),
		projectionEvent(t, 1, domain.SessionEventUserMessage, at.Add(time.Second), map[string]any{
			"turn": 0, "turn_id": "task-old:user", "task_id": "task-old", "agent_id": "a1",
			"content": historyQuestion + " 读一下 notes.md",
		}),
		projectionEvent(t, 2, domain.SessionEventStepStart, at.Add(2*time.Second), map[string]any{"turn": 0, "step": 0}),
		projectionEvent(t, 3, domain.SessionEventAssistantMessage, at.Add(3*time.Second), map[string]any{
			"turn": 0, "step": 0, "turn_id": "task-old:assistant", "task_id": "task-old", "agent_id": "a1",
			"content":       "我读一下",
			"model_profile": "fast",
			"tool_calls": []any{
				map[string]any{"call_id": historyCallID, "name": "read_file"},
			},
		}),
		projectionEvent(t, 4, domain.SessionEventToolCall, at.Add(4*time.Second), map[string]any{
			"turn": 0, "step": 0, "call_id": historyCallID, "name": "read_file",
			"arguments": "{\"path\":\"notes.md\"}",
		}),
		projectionEvent(t, 5, domain.SessionEventToolResult, at.Add(5*time.Second), map[string]any{
			"turn": 0, "step": 0, "call_id": historyCallID,
			"preview": historyToolOutput, "is_error": false,
		}),
		projectionEvent(t, 6, domain.SessionEventStepEnd, at.Add(6*time.Second), map[string]any{
			"turn": 0, "step": 0, "reason": "completed",
		}),
		projectionEvent(t, 7, domain.SessionEventStepStart, at.Add(7*time.Second), map[string]any{"turn": 0, "step": 1}),
		projectionEvent(t, 8, domain.SessionEventAssistantMessage, at.Add(8*time.Second), map[string]any{
			"turn": 0, "step": 1, "turn_id": "task-old:assistant", "task_id": "task-old", "agent_id": "a1",
			"content": historyAnswer + " 读完了", "model_profile": "fast",
		}),
		projectionEvent(t, 9, domain.SessionEventStepEnd, at.Add(9*time.Second), map[string]any{
			"turn": 0, "step": 1, "reason": "completed",
		}),
		projectionEvent(t, 10, domain.SessionEventTurnEnd, at.Add(10*time.Second), map[string]any{
			"turn": 0, "reason": "completed",
		}),
	}
	if err := repo.Append(ctx, sessionID, events); err != nil {
		t.Fatalf("append history events: %v", err)
	}
}

// runTaskWithHistory 把上面那段历史写进一个真的 SQLite 存储，用**生产两条任务路径
// 用的同一个选路**（SessionHistoryForTask，由 toolTranscript 决定形状）把它读出来，
// 然后跑一个后续任务，返回模型看到的那次交流。
//
// 它接的是真的 cognitive.Core：渲染 "Recent conversation:" 那段文本的就是 Core，
// 没有 ContextBuilder 的 Runtime 只会把 task.Input 塞进 message[0]，那时「关闭时
// 有那段文本」就成了一条任何夹具都造不出反例的空断言。
func runTaskWithHistory(t *testing.T, toolTranscript bool) []port.InferenceMessage {
	t.Helper()
	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	seedHistorySession(t, ctx, repo, historySessionID)

	task := domain.Task{ID: "task-now", SessionID: historySessionID, AgentID: "a1", Input: "接着说"}
	sessionCfg := config.SessionConfig{
		Enabled:               true,
		DefaultRecentTurns:    6,
		MaxTurnChars:          6000,
		ToolTranscriptEnabled: toolTranscript,
	}
	history, err := SessionHistoryForTask(ctx, repo, sessionCfg, task)
	if err != nil {
		t.Fatalf("SessionHistoryForTask: %v", err)
	}

	maas := &recordingMaas{}
	rt := NewRuntime(Config{
		Gate:  taskgate.NewTaskGate(),
		Maas:  maas,
		Audit: adapter.NewMemoryAuditLog(),
		// 给一段非空的稳定节，message[0] 才真有前缀可钉：稳定节为空时
		// stablePrefixLen 是 0，缓存断点那条断言会空过。
		ContextBuilder:    cognitive.NewCore(cognitive.NoopCompressor{}).WithContextFiles("项目约定：先读再改。"),
		MaxToolRounds:     3,
		ConversationTurns: history.Turns,
		HistoryTranscript: history.Transcript,
	})
	if _, err := rt.RunTask(ctx, domain.Agent{ID: "a1", Role: "developer"}, task); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	return maas.first(t).Messages
}

// 关闭时（默认）行为必须与今天逐字节相同：历史仍是 prompt 里那段
// "Recent conversation:" 文本块，messages 里没有任何 tool 角色。
//
// 这条守的是 spec §3 那句「不该在做轨迹的顺路上悄悄打开」。
func TestWithTheSwitchOffHistoryStaysInThePromptText(t *testing.T) {
	t.Parallel()

	msgs := runTaskWithHistory(t, false)

	if len(msgs) == 0 {
		t.Fatal("送给模型的 messages 是空的")
	}
	for i, m := range msgs {
		if m.Role == port.RoleTool {
			t.Errorf("messages[%d] 是 tool 角色：开关关着，历史不该以 transcript 进模型", i)
		}
	}
	if !strings.Contains(msgs[0].Content, "Recent conversation:") {
		t.Errorf("messages[0] 里没有 \"Recent conversation:\"：关闭时历史必须仍在 prompt 文本里\n%s", msgs[0].Content)
	}
	if !strings.Contains(msgs[0].Content, historyQuestion) {
		t.Errorf("messages[0] 里没有历史标记 %q：历史根本没注进去，上面那条断言就是白过的", historyQuestion)
	}
}

// 打开时历史变成 transcript：出现 tool 角色的消息，且每条都能在它前面找到
// 宣告它的 assistant。
func TestWithTheSwitchOnHistoryBecomesATranscript(t *testing.T) {
	t.Parallel()

	msgs := runTaskWithHistory(t, true)

	sawTool := false
	for _, m := range msgs {
		if m.Role == port.RoleTool {
			sawTool = true
			if !strings.Contains(m.Content, historyToolOutput) {
				t.Errorf("tool 消息的正文里没有历史工具输出 %q：%q", historyToolOutput, m.Content)
			}
		}
	}
	if !sawTool {
		t.Fatalf("开关打开却一条 tool 消息都没有：历史的工具往返没进模型\n%+v", msgs)
	}
	assertTranscriptIsValid(t, msgs)
	// provider 的硬性契约走一遍：配不上的 tool 消息会让整个请求被拒收。
	if err := (port.InferenceRequest{RequestID: "r", Messages: msgs}).Validate(); err != nil {
		t.Errorf("组装出来的请求发不出去：%v", err)
	}
	if strings.Contains(msgs[0].Content, "Recent conversation:") {
		t.Errorf("打开时 prompt 里还留着 \"Recent conversation:\"：历史进了两次，体积白涨一倍\n%s", msgs[0].Content)
	}
	if !strings.Contains(msgs[0].Content, "接着说") {
		t.Error("messages[0] 里没有当前任务的输入：排布错了")
	}
}

// 缓存断点不能被这条改动挪走：StablePrefixLen 仍打在 messages[0]，
// 且 messages[0] 仍以那段稳定前缀开头。
func TestTheCacheBreakpointStaysOnTheFirstMessage(t *testing.T) {
	t.Parallel()

	msgs := runTaskWithHistory(t, true)

	if msgs[0].StablePrefixLen <= 0 {
		t.Fatalf("messages[0].StablePrefixLen = %d，要 > 0：缓存断点没打在第一条上", msgs[0].StablePrefixLen)
	}
	prefix := string([]rune(msgs[0].Content)[:msgs[0].StablePrefixLen])
	if !strings.HasPrefix(prefix, "Runtime context files:") {
		t.Errorf("messages[0] 的稳定前缀不是那段跨任务相同的文本，而是 %q", prefix)
	}
	for i, m := range msgs[1:] {
		if m.StablePrefixLen != 0 {
			t.Errorf("messages[%d].StablePrefixLen = %d，要 0：那个字段只在 messages[0] 上有意义",
				i+1, m.StablePrefixLen)
		}
	}
}

// assertTranscriptIsValid 是上面几条共用的断言：每条 tool 消息前面都有宣告它的
// assistant。provider 拒收配不上的 tool 消息（port.InferenceMessage 的 Validate
// 注释），所以这是「能不能发出去」的判据，不是风格问题。
func assertTranscriptIsValid(t *testing.T, msgs []port.InferenceMessage) {
	t.Helper()
	announced := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case port.RoleAssistant:
			for _, c := range m.ToolCalls {
				announced[c.ID] = true
			}
		case port.RoleTool:
			if strings.TrimSpace(m.ToolCallID) == "" {
				t.Fatalf("msgs[%d] 是 tool 但没有 tool_call_id——provider 会拒收整个请求", i)
			}
			if !announced[m.ToolCallID] {
				t.Errorf("msgs[%d] 的 tool_call_id=%q 之前没有 assistant 宣告过", i, m.ToolCallID)
			}
		}
	}
}
