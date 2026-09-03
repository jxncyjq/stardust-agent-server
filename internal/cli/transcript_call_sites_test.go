package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	agentruntime "github.com/stardust/legion-agent/internal/runtime"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/tool"
)

// 这一组守 P5 Task 3 复审的 🔴 C1 与 🟠 I1 在 cli 侧的两处接线。
//
// C1（defaultTaskRunner）：复审把 command.go 里那行
// `runtimeCfg.HistoryTranscript = history.Transcript` 删掉，`go test ./...` 全绿。
// I1（legion tui）：这条生产路径当时**根本没接** G3——用户打开开关，serve 生效、tui
// 静默不生效，不报任何错。两者是同一个形状：接缝在，但那条路上没人把它接上。
//
// 两条测试断言的都是**装配的结果**（模型真正收到的 messages），不是「代码里有那一行」。

const (
	cliHistoryQuestion   = "CLI-HISTORY-QUESTION"
	cliHistoryToolOutput = "CLI-HISTORY-TOOL-OUTPUT"
	cliHistoryAnswer     = "CLI-HISTORY-ANSWER"
	cliHistoryCallID     = "cli-hist-c1"
)

// cliMessagesRecordingMaas 留下每次请求的完整 messages，并恒回一句纯文本。
//
// 按请求**内容**作答，绝不按次数：它一次工具都不要，没有可变的东西需要按次数分支。
type cliMessagesRecordingMaas struct {
	mu       sync.Mutex
	requests []port.InferenceRequest
}

func (m *cliMessagesRecordingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	return port.InferenceResponse{Text: "好的"}, nil
}

func (m *cliMessagesRecordingMaas) first(t *testing.T) []port.InferenceMessage {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		t.Fatal("假模型一次都没被调用：这次任务根本没发出模型请求")
	}
	return m.requests[0].Messages
}

// cliTranscriptOnlyLister 只服务 G3 打开那条路；turns 那条主动报错，这样「悄悄退回
// 旧形状」不会伪装成「这条会话本来就没历史」（CLAUDE.md §0）。
type cliTranscriptOnlyLister struct {
	transcript []port.InferenceMessage
	gotSession string
}

func (l *cliTranscriptOnlyLister) ListConversationTurns(_ context.Context, sessionID string, _ int) ([]domain.ConversationTurn, error) {
	return nil, fmt.Errorf("cliTranscriptOnlyLister: session %q asked for turns, but this double only serves the G3-on transcript path", sessionID)
}

func (l *cliTranscriptOnlyLister) ListConversationTranscript(_ context.Context, sessionID string, _ int) ([]port.InferenceMessage, error) {
	l.gotSession = sessionID
	return append([]port.InferenceMessage(nil), l.transcript...), nil
}

func cliHistoryWithOneToolRound() []port.InferenceMessage {
	return []port.InferenceMessage{
		{Role: port.RoleUser, Content: cliHistoryQuestion},
		{Role: port.RoleAssistant, Content: "我读一下", ToolCalls: []domain.ToolCall{
			{ID: cliHistoryCallID, Name: "read_file", Arguments: map[string]string{"path": "notes.md"}},
		}},
		{Role: port.RoleTool, ToolCallID: cliHistoryCallID, Content: cliHistoryToolOutput},
		{Role: port.RoleAssistant, Content: cliHistoryAnswer},
	}
}

// assertCLIModelSawTheToolRoundTrip：模型收到的那次请求里，历史的工具往返真的在，
// 且每条 tool 消息都配得上前面宣告它的 assistant（配不上 provider 拒收整个请求）。
func assertCLIModelSawTheToolRoundTrip(t *testing.T, msgs []port.InferenceMessage, callSite string) {
	t.Helper()
	sawTool := false
	announced := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case port.RoleAssistant:
			for _, c := range m.ToolCalls {
				announced[c.ID] = true
			}
		case port.RoleTool:
			if strings.Contains(m.Content, cliHistoryToolOutput) {
				sawTool = true
			}
			if !announced[m.ToolCallID] {
				t.Errorf("msgs[%d] 的 tool_call_id=%q 之前没有 assistant 宣告过——provider 会拒收整个请求",
					i, m.ToolCallID)
			}
		}
	}
	if !sawTool {
		t.Fatalf("%s：开关打开，模型却一条历史 tool 消息都没收到——这条路没把 transcript 接上，"+
			"而它不会报任何错\n%+v", callSite, msgs)
	}
}

// TestDefaultTaskRunnerSendsHistoryAsATranscript 守生产接线点①：
// defaultTaskRunner.RunTask 里那行 `runtimeCfg.HistoryTranscript = history.Transcript`。
//
// defaultTaskRunner 服务的是每一个 AgentID 不在 registry 里的任务，也就是 GUI 默认
// agent 走的那条路（绝大多数任务）。删掉那一行，这条测试红。
func TestDefaultTaskRunnerSendsHistoryAsATranscript(t *testing.T) {
	t.Parallel()

	maas := &cliMessagesRecordingMaas{}
	lister := &cliTranscriptOnlyLister{transcript: cliHistoryWithOneToolRound()}
	runner := &defaultTaskRunner{
		runtimeCfg: agentruntime.Config{
			Gate:           taskgate.NewTaskGate(),
			Maas:           maas,
			Events:         adapter.NewMemoryEventBus(),
			ContextBuilder: cognitive.NewCore(cognitive.NoopCompressor{}),
			MaxToolRounds:  1,
		},
		contextRoot: t.TempDir(),
		audit:       adapter.NewMemoryAuditLog(),
		webOptions:  tool.WebToolOptions{},

		conversationTurns: lister,
		sessionCfg: config.SessionConfig{
			DefaultRecentTurns:    6,
			MaxTurnChars:          6000,
			ToolTranscriptEnabled: true,
		},
	}

	task := domain.Task{ID: "task-now", SessionID: "sess-1", Input: "接着说"}
	if _, err := runner.RunTask(context.Background(), domain.Agent{}, task); err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}

	if lister.gotSession != "sess-1" {
		t.Fatalf("lister 收到的 sessionID = %q, want sess-1：历史根本没被取", lister.gotSession)
	}
	assertCLIModelSawTheToolRoundTrip(t, maas.first(t), "defaultTaskRunner.RunTask")
}

// cliSessionEvent 造一条 TUI 测试用的会话事件。
func cliSessionEvent(t *testing.T, seq int64, typ domain.SessionEventType, data map[string]any) domain.SessionEvent {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", typ, err)
	}
	return domain.SessionEvent{
		Seq:  seq,
		Type: typ,
		Time: time.Date(2026, 9, 2, 10, 0, int(seq), 0, time.UTC),
		Data: raw,
	}
}

// TestTheTUIPathSendsHistoryAsATranscript 守生产接线点③——复审的 🟠 I1 指出这条路
// 当时**完全没接** G3。
//
// 它连着三处新接线：tuiSessionController.SessionHistory 的选路、runTUITask 把
// Transcript 转发进 tuiTaskRunConfig、以及 app.RunTaskOptions.HistoryTranscript 进
// runtime.Config。删掉其中任何一处，这条测试红。
//
// 历史直接写进事件日志（而不是用 recordTurn），因为 recordTurn 只写 user/assistant
// 两类事件，造不出工具往返——而工具往返正是 G3 的全部意义。
func TestTheTUIPathSendsHistoryAsATranscript(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openCLITestSQLiteRepository(t)
	sessionCfg := config.SessionConfig{
		Enabled:               true,
		DefaultRecentTurns:    6,
		MaxTurnChars:          6000,
		ToolTranscriptEnabled: true,
	}
	session := newTUISessionController(tuiSessionControllerConfig{
		Store:                 repo,
		Enabled:               sessionCfg.Enabled,
		CompanyID:             "cli-company",
		AgentID:               "cli-agent",
		ModelProfile:          "dev",
		RecentTurns:           sessionCfg.DefaultRecentTurns,
		MaxTurnChars:          sessionCfg.MaxTurnChars,
		ToolTranscriptEnabled: sessionCfg.ToolTranscriptEnabled,
	})
	if _, err := session.NewSession(ctx); err != nil {
		t.Fatalf("NewSession() error = %v, want nil", err)
	}

	seedCLIToolRoundHistory(t, ctx, repo, session.CurrentSessionID())

	maas := &cliMessagesRecordingMaas{}
	if _, err := runTUITask(ctx, app.New(), tuiTaskRunConfig{
		Config: config.Config{
			Runtime: config.RuntimeConfig{MaxToolRounds: 1},
			Session: sessionCfg,
		},
		Prompt: "接着说",
		// 故意留空：模拟 --no-context-files（或 context_files.enabled=false）。
		// G3 打开时 ConversationTurns 恒空（历史改走 Transcript），若 app.go:383 那个门
		// 仍拿 ConversationTurns 当条件，ContextPrefix 又空，cognitive Core 就整个不建——
		// 这条测试原来留空且不断言 header，因此在那种坏配置下也照样绿（空断言）。现在下面
		// 显式断言 header 仍在，就必须靠 HistoryTranscript 撑住这个门。
		DefaultContextPrefix: "",
		DefaultMaas:          maas,
		Session:              session,
	}); err != nil {
		t.Fatalf("runTUITask() error = %v, want nil", err)
	}

	msgs := maas.first(t)
	assertCLIModelSawTheToolRoundTrip(t, msgs, "runTUITask")
	// G3 打开时 cognitive Core 仍必须建：history.Transcript 非空也要让 app.go:383 那个门
	// 打开，否则模型收不到 "Agent:"/"Role:"/"Task:"/"Tools:" 这个任务框架头，且不报任何错。
	if !strings.Contains(msgs[0].Content, "Agent: cli-agent") ||
		!strings.Contains(msgs[0].Content, "Tools: ") {
		t.Errorf("打开时丢了 cognitive Core 的任务框架 header（Agent:/Role:/Task:/Tools:）\n%s", msgs[0].Content)
	}
	// 历史不能同时再走一遍文本块：那会让同一段历史进两次，体积白涨一倍。
	if strings.Contains(msgs[0].Content, "Recent conversation:") {
		t.Errorf("打开时 prompt 里还留着 \"Recent conversation:\"：历史进了两次\n%s", msgs[0].Content)
	}
}

// seedCLIToolRoundHistory 往 sessionID 的事件日志里写一个**已完成的上一个任务**：
// 问一句、一轮调用 read_file 的 assistant、那次调用的结果、收尾的回答。
//
// 直接写事件而不是用 recordTurn，因为后者只写 user/assistant 两类事件，造不出工具
// 往返——而工具往返正是 G3 的全部意义：一段没有工具调用的历史在开与关两种设置下投影
// 成同一个形状，什么也证明不了。
func seedCLIToolRoundHistory(t *testing.T, ctx context.Context, repo conversationStore, sessionID string) {
	t.Helper()
	if err := repo.Append(ctx, sessionID, []domain.SessionEvent{
		cliSessionEvent(t, 0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		cliSessionEvent(t, 1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "task-old:user", "task_id": "task-old", "agent_id": "cli-agent",
			"content": cliHistoryQuestion,
		}),
		cliSessionEvent(t, 2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		cliSessionEvent(t, 3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "task-old:assistant", "task_id": "task-old",
			"agent_id": "cli-agent", "content": "我读一下", "model_profile": "dev",
			"tool_calls": []any{map[string]any{"call_id": cliHistoryCallID, "name": "read_file"}},
		}),
		cliSessionEvent(t, 4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": cliHistoryCallID, "name": "read_file",
			"arguments": `{"path":"notes.md"}`,
		}),
		cliSessionEvent(t, 5, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": cliHistoryCallID,
			"preview": cliHistoryToolOutput, "is_error": false,
		}),
		cliSessionEvent(t, 6, domain.SessionEventStepEnd, map[string]any{
			"turn": 0, "step": 0, "reason": "completed",
		}),
		cliSessionEvent(t, 7, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 1}),
		cliSessionEvent(t, 8, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 1, "turn_id": "task-old:assistant2", "task_id": "task-old",
			"agent_id": "cli-agent", "content": cliHistoryAnswer, "model_profile": "dev",
		}),
		cliSessionEvent(t, 9, domain.SessionEventStepEnd, map[string]any{
			"turn": 0, "step": 1, "reason": "completed",
		}),
		cliSessionEvent(t, 10, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "completed"}),
	}); err != nil {
		t.Fatalf("append history events: %v", err)
	}
}

// TestTheTUISessionControllerCarriesTheTranscriptSwitch 守 `legion tui` 的装配那一跳：
// buildTUISessionController 必须把 cfg.Session.ToolTranscriptEnabled 交给控制器。
//
// 断言的是**装配的结果**（控制器真的返回了 transcript 形状），不是「代码里有那一行」：
// 把那一行删掉，控制器会安静地退回 turns 形状，而 turns 那条路在这个夹具里是可用的，
// 不会报任何错。
func TestTheTUISessionControllerCarriesTheTranscriptSwitch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openCLITestSQLiteRepository(t)
	cfg := config.Config{Session: config.SessionConfig{
		Enabled:               true,
		DefaultRecentTurns:    6,
		MaxTurnChars:          6000,
		ToolTranscriptEnabled: true,
	}}

	session := buildTUISessionController(cfg, repo, "dev")
	if _, err := session.NewSession(ctx); err != nil {
		t.Fatalf("NewSession() error = %v, want nil", err)
	}
	seedCLIToolRoundHistory(t, ctx, repo, session.CurrentSessionID())

	history, err := session.SessionHistory(ctx)
	if err != nil {
		t.Fatalf("SessionHistory() error = %v, want nil", err)
	}
	if len(history.Transcript) == 0 {
		t.Fatal("装配出来的控制器仍然走 turns：cfg.Session.ToolTranscriptEnabled 没被交给它，" +
			"`legion tui` 上这个开关就是死的")
	}
	if len(history.Turns) != 0 {
		t.Errorf("两半同时非空：同一段历史会进两次\n%+v", history.Turns)
	}
}

// TestTheMentionedAgentTUIPathSendsHistoryAsATranscript 守 TUI 的**第二个**任务入口：
// runMentionedTUIAgentTask 也要把 cfg.HistoryTranscript 转发进 app.RunTaskOptions。
//
// 只守 runTUITask 那一处是不够的：两个入口各有一份 RunTaskOptions 字面量，@提及那条
// 漏掉这一行，被 @ 的 agent 就会在 G3 打开时静默丢掉历史——与 ToolGate 当年那处
// 「@mention 路径没接」是同一个形状（见 tuiTaskRunConfig.ToolGate 的注释）。
//
// 断言落在假 provider 收到的**请求体**上：@提及那条路的客户端是按 agent 自己的档位
// 现建的真 HTTP 客户端，测试拿不到它的 port.InferenceRequest。
func TestTheMentionedAgentTUIPathSendsHistoryAsATranscript(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var mu sync.Mutex
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if body == nil {
			var got map[string]any
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			body = got
		}
		mu.Unlock()
		writeSSEChoice(t, w, map[string]any{"content": "好的"})
	}))
	t.Cleanup(server.Close)

	repo := openCLITestSQLiteRepository(t)
	sessionCfg := config.SessionConfig{
		Enabled:               true,
		DefaultRecentTurns:    6,
		MaxTurnChars:          6000,
		ToolTranscriptEnabled: true,
	}
	cfg := config.Config{
		Maas: config.MaasConfig{
			DefaultProfile: "review",
			Profiles: map[string]config.MaasProfile{
				"review": {BaseURL: server.URL, Model: "deepseek-reasoner"},
			},
		},
		Runtime: config.RuntimeConfig{MaxToolRounds: 1},
		Session: sessionCfg,
	}
	session := buildTUISessionController(cfg, repo, "review")
	if _, err := session.NewSession(ctx); err != nil {
		t.Fatalf("NewSession() error = %v, want nil", err)
	}
	seedCLIToolRoundHistory(t, ctx, repo, session.CurrentSessionID())

	if _, err := runTUITask(ctx, app.New(), tuiTaskRunConfig{
		Config: cfg,
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {ID: "researcher", Role: "researcher", MaasProfile: "review"},
		}),
		Prompt:  "@researcher 接着说",
		Session: session,
	}); err != nil {
		t.Fatalf("runTUITask(@researcher) error = %v, want nil", err)
	}

	mu.Lock()
	got := body
	mu.Unlock()
	assertWireCarriesTheHistoryToolMessage(t, got)
}

// assertWireCarriesTheHistoryToolMessage 在假 provider 收到的请求体里找那条历史 tool
// 消息，并核对它配得上前面宣告它的 assistant。
func assertWireCarriesTheHistoryToolMessage(t *testing.T, body map[string]any) {
	t.Helper()
	if body == nil {
		t.Fatal("假 provider 一次请求都没收到")
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("请求体里没有 messages：%+v", body)
	}
	announced := map[string]bool{}
	sawTool := false
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, rawCall := range calls {
				if call, ok := rawCall.(map[string]any); ok {
					if id, ok := call["id"].(string); ok {
						announced[id] = true
					}
				}
			}
		}
		if msg["role"] != "tool" {
			continue
		}
		content, _ := msg["content"].(string)
		if !strings.Contains(content, cliHistoryToolOutput) {
			continue
		}
		sawTool = true
		id, _ := msg["tool_call_id"].(string)
		if !announced[id] {
			t.Errorf("线上那条 tool 消息 tool_call_id=%q 之前没有 assistant 宣告过——provider 会拒收整个请求", id)
		}
	}
	if !sawTool {
		t.Fatalf("@提及那条路上，历史的工具往返一条都没进请求体——这个入口没转发 HistoryTranscript\n%+v",
			messages)
	}
}

// TestTheTUIPathKeepsTheTextBlockWhenTheSwitchIsOff 是上一条的反面：开关关着时 TUI
// 必须与今天逐字节相同——历史仍是 prompt 里那段文本，一条 tool 消息都不该出现。
//
// 没有它，「把 SessionHistory 写成恒走 transcript」这类改动不会被任何测试挡住。
func TestTheTUIPathKeepsTheTextBlockWhenTheSwitchIsOff(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openCLITestSQLiteRepository(t)
	session := newTUISessionController(tuiSessionControllerConfig{
		Store:        repo,
		Enabled:      true,
		CompanyID:    "cli-company",
		AgentID:      "cli-agent",
		ModelProfile: "dev",
		RecentTurns:  6,
		MaxTurnChars: 6000,
	})
	if _, err := session.NewSession(ctx); err != nil {
		t.Fatalf("NewSession() error = %v, want nil", err)
	}
	if err := session.recordTurn(ctx, domain.ConversationRoleUser, "task-old", "cli-agent", "dev", cliHistoryQuestion); err != nil {
		t.Fatalf("record user turn: %v", err)
	}
	if err := session.recordTurn(ctx, domain.ConversationRoleAssistant, "task-old", "cli-agent", "dev", cliHistoryAnswer); err != nil {
		t.Fatalf("record assistant turn: %v", err)
	}

	maas := &cliMessagesRecordingMaas{}
	if _, err := runTUITask(ctx, app.New(), tuiTaskRunConfig{
		Config: config.Config{
			Runtime: config.RuntimeConfig{MaxToolRounds: 1},
			Session: config.SessionConfig{Enabled: true, DefaultRecentTurns: 6, MaxTurnChars: 6000},
		},
		Prompt:      "接着说",
		DefaultMaas: maas,
		Session:     session,
	}); err != nil {
		t.Fatalf("runTUITask() error = %v, want nil", err)
	}

	msgs := maas.first(t)
	if len(msgs) != 1 {
		t.Fatalf("关闭时送给模型的 messages 有 %d 条，要 1 条：历史被当消息追加了\n%+v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0].Content, "Recent conversation:") {
		t.Errorf("messages[0] 里没有 \"Recent conversation:\"：关闭时历史必须仍在 prompt 文本里\n%s", msgs[0].Content)
	}
	if !strings.Contains(msgs[0].Content, cliHistoryQuestion) {
		t.Errorf("messages[0] 里没有历史标记 %q：历史根本没注进去，上面那条断言就是白过的",
			cliHistoryQuestion)
	}
}
