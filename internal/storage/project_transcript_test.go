package storage

import (
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// 一轮「问 → 调工具 → 答」投影成 provider transcript 的标准形状：
// user、assistant(带 tool_calls)、tool(带 tool_call_id)、assistant。
func TestOneToolRoundProjectsToAValidTranscript(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "读 notes.md",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content": "我读一下",
			"tool_calls": []any{
				map[string]any{"call_id": "c1", "name": "read_file"},
			},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": `{"path":"notes.md"}`,
		}),
		evWith(5, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "文件的前几行", "is_error": false,
		}),
		evWith(6, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "completed"}),
		evWith(7, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 1}),
		evWith(8, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 1, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content": "读完了", "tool_calls": []any{},
			"usage":         map[string]any{"prompt": 2, "completion": 1, "cached": 0, "total": 3},
			"model_profile": "fast",
		}),
		evWith(9, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 1, "reason": "completed"}),
		evWith(10, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "completed"}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	if len(msgs) != 4 {
		t.Fatalf("投影出 %d 条消息，要 4 条（user / assistant+calls / tool / assistant）：%+v", len(msgs), msgs)
	}
	if msgs[0].Role != port.RoleUser || msgs[0].Content != "读 notes.md" {
		t.Errorf("msgs[0] = %+v，要 user「读 notes.md」", msgs[0])
	}
	if msgs[1].Role != port.RoleAssistant {
		t.Fatalf("msgs[1].Role = %q，要 assistant", msgs[1].Role)
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].ID != "c1" {
		t.Errorf("msgs[1].ToolCalls = %+v，要一条 call_id=c1", msgs[1].ToolCalls)
	}
	if msgs[2].Role != port.RoleTool || msgs[2].ToolCallID != "c1" {
		t.Errorf("msgs[2] = %+v，要 tool 且 tool_call_id=c1", msgs[2])
	}
	if msgs[3].Role != port.RoleAssistant || msgs[3].Content != "读完了" {
		t.Errorf("msgs[3] = %+v，要 assistant「读完了」", msgs[3])
	}
}

// spec §4.3.1 第 2 条：崩溃恢复补出的 tool/result 排在日志**尾部**，可能排在
// step/end、turn/end 之后，而且顺序与调用顺序相反。按位置配会把结果配到错的
// 调用上；必须按 call_id 配。
//
// provider 会拒绝配不上的 tool 消息（ports.go 的 Validate 注释写死了），
// 所以这不是审美问题——配错就发不出去。
func TestResultsPairByCallIDEvenWhenTheyTrailAtTheEnd(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "并行读两个文件",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content": "同时读",
			"tool_calls": []any{
				map[string]any{"call_id": "c1", "name": "read_file"},
				map[string]any{"call_id": "c2", "name": "read_file"},
			},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
		evWith(5, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "name": "read_file", "arguments": "{}",
		}),
		// 崩了。恢复补出来的两条结果排在尾部，且顺序与调用相反。
		evWith(6, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "preview": "c2 的内容", "is_error": true,
		}),
		evWith(7, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "c1 的内容", "is_error": true,
		}),
		evWith(8, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "interrupted"}),
		evWith(9, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "interrupted"}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	// 找出两条 tool 消息，按 call_id 核对内容——**按位置配的实现会在这里把
	// c1 的内容配给 c2**。
	byCallID := map[string]string{}
	for _, m := range msgs {
		if m.Role == port.RoleTool {
			byCallID[m.ToolCallID] = m.Content
		}
	}
	if len(byCallID) != 2 {
		t.Fatalf("tool 消息 %d 条，要 2 条：%+v", len(byCallID), msgs)
	}
	if !strings.Contains(byCallID["c1"], "c1 的内容") {
		t.Errorf("c1 配到了 %q——按位置配对会把尾部乱序的结果配错", byCallID["c1"])
	}
	if !strings.Contains(byCallID["c2"], "c2 的内容") {
		t.Errorf("c2 配到了 %q", byCallID["c2"])
	}
}

// 每条 tool 消息前面必须有带同 call_id 的 assistant tool_calls，否则
// provider 拒收整个请求（ports.go 的 Validate 注释）。这条把「合法 transcript」
// 当成不变量来验，而不是只看字段填没填。
func TestEveryToolMessageIsPrecededByItsCall(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "干活",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "调工具",
			"tool_calls":    []any{map[string]any{"call_id": "c1", "name": "read_file"}},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
		evWith(5, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "结果", "is_error": false,
		}),
		evWith(6, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "completed"}),
		evWith(7, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "completed"}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	announced := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case port.RoleAssistant:
			for _, c := range m.ToolCalls {
				announced[c.ID] = true
			}
		case port.RoleTool:
			if m.ToolCallID == "" {
				t.Fatalf("msgs[%d] 是 tool 但没有 tool_call_id——provider 会拒收整个请求", i)
			}
			if !announced[m.ToolCallID] {
				t.Errorf("msgs[%d] 的 tool_call_id=%q 之前没有任何 assistant 宣告过它——provider 会拒收", i, m.ToolCallID)
			}
		}
	}
}

// 一条被记录过、但没有结果事件的调用只可能来自进程硬崩（spec §4.3.1 第 1 条）。
// 它不能被原样放进 transcript——那会产出一条永远等不到 tool 消息的 tool_calls，
// provider 同样拒收。
func TestAnUnansweredCallIsNotAnnouncedInTheTranscript(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "干活",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content": "调两个",
			"tool_calls": []any{
				map[string]any{"call_id": "c1", "name": "read_file"},
				map[string]any{"call_id": "c2", "name": "read_file"},
			},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
		evWith(5, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "name": "read_file", "arguments": "{}",
		}),
		// 只有 c1 有结果；c2 是硬崩留下的未答调用。
		evWith(6, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "c1 的结果", "is_error": false,
		}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	for i, m := range msgs {
		if m.Role != port.RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls {
			if c.ID == "c2" {
				t.Errorf("msgs[%d] 宣告了没有结果的 c2——provider 会等一条永远不来的 tool 消息", i)
			}
		}
	}
}

// 内容是**预览**不是全文（spec §6：全文仍靠 read_file 按定位符取）。
// 这条守的是 G3 的成本上界：把全文塞进每次请求正是它要避免的事。
//
// 判据是「投影不放大」：产出 ≤ 预览长度 + 定位符长度 + 一个常数尾巴，而不是某个与
// 写入侧无关的魔数。夹具喂一条**贴着写入侧上限**的预览（internal/runtime 的
// maxEventPreviewRunes = 2000，跨包引不到，这里按同值复刻并写明来源）：喂一条 6 个
// 字的预览再断言「不超过 500」，只能证明「短的还是短的」——将来投影侧真的开始展开
// 全文，只要单条预览不超 500 rune 它照样不红，护不住它声称的东西。
func TestToolContentIsThePreviewNotTheWholeText(t *testing.T) {
	// 与 internal/runtime.maxEventPreviewRunes 同值：预览在写入侧就已经截到这里，
	// 投影侧拿到的最长就是这么长。
	const maxEventPreviewRunes = 2000
	// 投影允许在预览外多写的东西：is_error 前缀加定位符那行的固定文字。
	const renderOverheadRunes = 40
	preview := strings.Repeat("每条预览都可能顶着写入侧的上限", maxEventPreviewRunes/15)
	const locator = ".stardust/cache/read_file-abc.md"
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "读",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "读",
			"tool_calls":    []any{map[string]any{"call_id": "c1", "name": "read_file"}},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
		evWith(5, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": preview, "is_error": false,
			"spill_locator": locator,
		}),
		evWith(6, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "completed"}),
		evWith(7, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "completed"}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	for _, m := range msgs {
		if m.Role != port.RoleTool {
			continue
		}
		if !strings.Contains(m.Content, preview) {
			t.Errorf("tool 消息没有带预览：%q", m.Content)
		}
		// 定位符可以出现（让模型知道去哪取全文），但全文本身绝不能在这里：
		// 投影只许在预览之上加那点固定尾巴，不许放大。
		if !strings.Contains(m.Content, locator) {
			t.Errorf("tool 消息没有带定位符，模型不知道去哪取全文：%q", m.Content)
		}
		want := len([]rune(preview)) + len([]rune(locator)) + renderOverheadRunes
		if got := len([]rune(m.Content)); got > want {
			t.Errorf("tool 消息 %d runes > 预览 %d + 定位符 %d + 尾巴 %d = %d——"+
				"投影把内容放大了，G3 的成本上界就是靠「只放预览」守的",
				got, len([]rune(preview)), len([]rune(locator)), renderOverheadRunes, want)
		}
	}
}

// 未知事件类型必须报错并指名，不能静默跳过——server 侧的类型是闭集但它会长，
// 静默跳过意味着加了新类型后 transcript 会悄悄少东西。
func TestAnUnknownEventTypeIsRefusedByNameInTheTranscript(t *testing.T) {
	_, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventType("session/teleport"), map[string]any{"turn": 0}),
	})
	if err == nil {
		t.Fatal("未知事件类型没有被拒绝")
	}
	if !strings.Contains(err.Error(), "session/teleport") {
		t.Errorf("错误没有指名那个未知类型：%v", err)
	}
}

// 空 call_id 的宣告必须被拒绝并指名 seq，不能当成「未答调用」悄悄跳过。
//
// 这条不在 brief 里，是自我复审补的：光有「只宣告有结果的调用」那条 continue，
// 一条 call_id 为空的宣告会走进同一条 continue——因为 results 里永远不会有空键
// （collectToolResults 已经拒绝空 call_id 的结果）。于是「写入方坏了」与「进程
// 硬崩留下未答调用」这两件性质完全不同的事被折叠成同一种无声后果：调用从
// transcript 里消失、没人知道。recordAssistantMessage 原样抄 domain.ToolCall.ID，
// 空值只可能来自坏掉的写入方，属 fail-loud 铁律里的「本不该发生」。
func TestAnAnnouncedCallWithAnEmptyCallIDIsRefused(t *testing.T) {
	_, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "干活",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "调工具",
			"tool_calls":    []any{map[string]any{"call_id": "", "name": "read_file"}},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
	})
	if err == nil {
		t.Fatal("空 call_id 的宣告没有被拒绝——它会被当成未答调用悄悄跳过")
	}
	if !strings.Contains(err.Error(), "call_id") || !strings.Contains(err.Error(), "seq 3") {
		t.Errorf("错误没有指明缺的是 call_id 以及是哪条事件：%v", err)
	}
}

// 空 call_id 的**结果**同样必须被拒绝并指名 seq：它配不上任何调用，
// 而无声丢掉它意味着那次调用会被当成「未答」而整个从 transcript 里消失。
func TestAToolResultWithAnEmptyCallIDIsRefused(t *testing.T) {
	_, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "", "preview": "结果", "is_error": false,
		}),
	})
	if err == nil {
		t.Fatal("空 call_id 的 tool/result 没有被拒绝")
	}
	if !strings.Contains(err.Error(), "call_id") || !strings.Contains(err.Error(), "seq 1") {
		t.Errorf("错误没有指明缺的是 call_id 以及是哪条事件：%v", err)
	}
}

// 载荷是坏 JSON 时必须报错并指名事件类型与 seq，不能当成空载荷接着投影。
func TestACorruptPayloadIsRefusedByTypeAndSeq(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  domain.SessionEventType
		seq  int64
	}{
		{"user/message", domain.SessionEventUserMessage, 1},
		{"assistant/message", domain.SessionEventAssistantMessage, 2},
		{"tool/result", domain.SessionEventToolResult, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := projectTranscript("s", []domain.SessionEvent{
				evRaw(tc.seq, tc.typ, []byte(`{"content":`)),
			})
			if err == nil {
				t.Fatalf("%s 的坏 JSON 载荷没有被拒绝", tc.name)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("错误没有指名事件类型 %s：%v", tc.name, err)
			}
		})
	}
}

// 🔴 Critical-1 的守卫：同一条会话里两轮工具循环**都用 call_1**，每条 tool 消息
// 必须拿到**自己那一轮**的结果。
//
// 这不是理论场景：spec §4.3.1 第 4 条明文允许跨 turn/step 复用 call_id（provider 的
// tool call id 只保证单次响应内唯一，按序号生成 call_1 / tooluse_0 的实现是存在的），
// 而 runtime 的 disambiguateCallIDs 只在单轮内去重——它的 used/arrived 两张表都是
// 每次调用新建的局部变量。所以下面这份日志是合法且预期的，会原样落盘。
//
// 把配对键退回成会话级的 map[string]string（只按 call_id），第一轮的 tool 消息会拿到
// 第二轮的结果，而且 port.InferenceRequest.Validate() 照样放行、provider 照单全收——
// 模型读到一条张冠李戴的历史工具输出，没有任何东西会报错。这条测试就是那道警报。
//
// 事件形状抄自一次真实 RunTask 的事件日志（同一 turn、两个 step、两轮都是 call_1）。
func TestTheSameCallIDInTwoRoundsPairsToItsOwnResult(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "先读再写",
		}),
		// 第一轮：call_1 = read_file。
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "先读",
			"tool_calls":    []any{map[string]any{"call_id": "call_1", "name": "read_file"}},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "call_1", "name": "read_file", "arguments": "{}",
		}),
		evWith(5, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "call_1", "preview": "READ 的结果", "is_error": false,
		}),
		evWith(6, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "completed"}),
		// 第二轮：同一个 call_1，这次是 write_file。
		evWith(7, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 1}),
		evWith(8, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 1, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "再写",
			"tool_calls":    []any{map[string]any{"call_id": "call_1", "name": "write_file"}},
			"usage":         map[string]any{"prompt": 2, "completion": 1, "cached": 0, "total": 3},
			"model_profile": "fast",
		}),
		evWith(9, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 1, "call_id": "call_1", "name": "write_file", "arguments": "{}",
		}),
		evWith(10, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 1, "call_id": "call_1", "preview": "WRITE 的结果", "is_error": false,
		}),
		evWith(11, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 1, "reason": "completed"}),
		evWith(12, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "completed"}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	// 两条 tool 消息按出现顺序就是两轮的顺序（每条紧随宣告它的 assistant）。
	var tools []port.InferenceMessage
	for _, m := range msgs {
		if m.Role == port.RoleTool {
			tools = append(tools, m)
		}
	}
	if len(tools) != 2 {
		t.Fatalf("tool 消息 %d 条，要 2 条（两轮各一条）：%+v", len(tools), msgs)
	}
	if !strings.Contains(tools[0].Content, "READ 的结果") {
		t.Errorf("第一轮的 tool 消息拿到了 %q，要「READ 的结果」——"+
			"会话级 call_id 配对键会让第二轮的结果覆盖第一轮的，而 Validate() 放行、provider 照收", tools[0].Content)
	}
	if !strings.Contains(tools[1].Content, "WRITE 的结果") {
		t.Errorf("第二轮的 tool 消息拿到了 %q，要「WRITE 的结果」", tools[1].Content)
	}
}

// 🟠 Important-1 的裁定：过滤掉全部未答调用之后什么都不剩的 assistant 消息，整条跳过。
//
// 模型只返工具调用、不返文本时 content 就是 ""（很常见）。若这一步的调用全部未答
// （硬崩），照样 append 会产出 {role:"assistant", content:"", tool_calls:nil}——
// OpenAI 兼容 provider 拒收这种消息，而 port.InferenceRequest.Validate() 管不到它
// （它只校验 role=tool 必须有 ToolCallID）。
//
// 裁定是跳过而不是塞占位内容：占位内容是替模型编一句它没说过的话。
func TestAnAssistantMessageWithNothingLeftToSayIsSkipped(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "干活",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			// 只调工具、不说话，然后硬崩：c1 没有结果。
			"content":       "",
			"tool_calls":    []any{map[string]any{"call_id": "c1", "name": "read_file"}},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	for i, m := range msgs {
		if m.Role != port.RoleAssistant {
			continue
		}
		if len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) == "" {
			t.Errorf("msgs[%d] 是一条既无 content 也无 tool_calls 的 assistant 消息——provider 拒收它", i)
		}
	}
	if len(msgs) != 1 || msgs[0].Role != port.RoleUser {
		t.Fatalf("投影出 %+v，要只剩那条 user 消息", msgs)
	}
}

// 有话说的 assistant 消息不会因为调用被过滤光就跟着消失——跳过的判据是「什么都不剩」，
// 不是「没有 tool_calls」。这条钉住上一条测试不会被实现成「有 tool_calls 才留」。
func TestAnAssistantMessageWithTextSurvivesWhenItsCallsAreFilteredOut(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "我先想想，顺手读一下",
			"tool_calls":    []any{map[string]any{"call_id": "c1", "name": "read_file"}},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "我先想想，顺手读一下" || len(msgs[0].ToolCalls) != 0 {
		t.Fatalf("投影出 %+v，要保留那条有正文的 assistant 消息、且不宣告未答的 c1", msgs)
	}
}

// 🟠 Important-2 的连带裁定：挂起→恢复会把同一条响应的同一批 tool_calls 记成两条
// assistant/message（runtime.go 的 resume 分支，挂起那一轮只记 step/end+turn/end，
// **不记** tool/call / tool/result）。会话级配对键下两条 assistant 都查得到那唯一的
// 结果，于是同一个 tool_call_id 在 transcript 里出现两次，token 成本翻倍。
//
// (turn, step, call_id) 键让挂起那一轮（turn 0）查不到任何结果——结果落在 turn 1——
// 于是它不宣告，重复自然消失。这条测试把这个连带效果钉住。
func TestAResumedTurnDoesNotAnnounceTheSameCallTwice(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		// 挂起的那一轮：记了响应，没来得及派发。
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "删掉临时目录",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "我来删",
			"tool_calls":    []any{map[string]any{"call_id": "c1", "name": "delete_path"}},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "cancelled"}),
		evWith(5, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "cancelled"}),
		// 恢复：新 turn 重记同一条响应，这次真的派发了。
		evWith(6, domain.SessionEventTurnStart, map[string]any{"turn": 1}),
		evWith(7, domain.SessionEventUserMessage, map[string]any{
			"turn": 1, "turn_id": "t:user", "task_id": "t", "content": "删掉临时目录",
		}),
		evWith(8, domain.SessionEventStepStart, map[string]any{"turn": 1, "step": 0}),
		evWith(9, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 1, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "我来删",
			"tool_calls":    []any{map[string]any{"call_id": "c1", "name": "delete_path"}},
			"usage":         map[string]any{"prompt": 0, "completion": 0, "cached": 0, "total": 0},
			"model_profile": "fast",
		}),
		evWith(10, domain.SessionEventToolCall, map[string]any{
			"turn": 1, "step": 0, "call_id": "c1", "name": "delete_path", "arguments": "{}",
		}),
		evWith(11, domain.SessionEventToolResult, map[string]any{
			"turn": 1, "step": 0, "call_id": "c1", "preview": "removed", "is_error": false,
		}),
		evWith(12, domain.SessionEventStepEnd, map[string]any{"turn": 1, "step": 0, "reason": "completed"}),
		evWith(13, domain.SessionEventTurnEnd, map[string]any{"turn": 1, "reason": "completed"}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	announced, answered := 0, 0
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if c.ID == "c1" {
				announced++
			}
		}
		if m.Role == port.RoleTool && m.ToolCallID == "c1" {
			answered++
		}
	}
	if announced != 1 || answered != 1 {
		t.Errorf("c1 被宣告 %d 次、被应答 %d 次，各要 1 次——挂起那一轮不该再宣告一遍：%+v",
			announced, answered, msgs)
	}
}

// 同一次调用出现两条结果是坏日志，必须报错而不是取后者。
//
// 它在合法路径上不可能发生：单轮内 call_id 由 runtime 的 disambiguateCallIDs 去重；
// 恢复对每个 call_id 至多合成一条结果，且只为**从未被应答**的调用合成
// （session_events.go 的 planRecovery）。所以撞键只可能是日志本身坏了，
// 「取后者接着跑」正是 CLAUDE.md §0 禁的兜底。
func TestTwoResultsForTheSameCallAreRefused(t *testing.T) {
	_, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "第一条", "is_error": false,
		}),
		evWith(1, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "第二条", "is_error": false,
		}),
	})
	if err == nil {
		t.Fatal("同一次调用的两条结果没有被拒绝——后者会静默覆盖前者")
	}
	if !strings.Contains(err.Error(), "c1") || !strings.Contains(err.Error(), "seq 1") {
		t.Errorf("错误没有指名 call_id 与那条重复事件：%v", err)
	}
}

// 🟡 Minor-2 的裁定：配不上任何宣告的 tool/result 不进 transcript，且这是**想清楚的**
// 决定，不是漏网。
//
// 一条没有 assistant 宣告过的 tool 消息本来就发不出去（provider 拒收配不上前面
// tool_calls 的 tool 消息，见 port.InferenceMessage 的文档注释），把它铺进去只会
// 让整个请求被拒。丢掉它产出的是一份**合法**的 transcript，与「未答的调用不宣告」
// 是同一类，不是给错误兜底。
//
// 这条测试的作用是把这个决定钉在代码里：下一个人重构 collectToolResults 时看得到
// 它是被想过的，而不是碰巧如此。
func TestAnOrphanResultIsNotProjected(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "调一个",
			"tool_calls":    []any{map[string]any{"call_id": "c1", "name": "read_file"}},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(2, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "c1 的结果", "is_error": false,
		}),
		// 谁也没宣告过它：没有任何 assistant/message 的 (turn, step) 上有这个 call_id。
		evWith(3, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 9, "call_id": "ORPHAN", "preview": "没人认领的结果", "is_error": false,
		}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}
	for i, m := range msgs {
		if m.ToolCallID == "ORPHAN" || strings.Contains(m.Content, "没人认领的结果") {
			t.Errorf("msgs[%d] = %+v：孤儿结果被铺进了 transcript，provider 会拒收整个请求", i, m)
		}
	}
	if len(msgs) != 2 {
		t.Fatalf("投影出 %d 条消息，要 2 条（assistant + c1 的 tool）：%+v", len(msgs), msgs)
	}
}

// turn/step 缺失必须报错并指名 seq：turn 0 / step 0 是合法坐标，缺字段的零值会伪装
// 成它们，配对键随之指到别处去——正是 CLAUDE.md §0 的「凑个值接着跑」。
func TestAMissingTurnOrStepIsRefused(t *testing.T) {
	t.Run("tool/result", func(t *testing.T) {
		_, err := projectTranscript("s", []domain.SessionEvent{
			evWith(1, domain.SessionEventToolResult, map[string]any{
				"turn": 0, "call_id": "c1", "preview": "结果", "is_error": false,
			}),
		})
		if err == nil {
			t.Fatal("缺 step 的 tool/result 没有被拒绝")
		}
		if !strings.Contains(err.Error(), "seq 1") || !strings.Contains(err.Error(), "turn/step") {
			t.Errorf("错误没有指明缺的是 turn/step 以及是哪条事件：%v", err)
		}
	})
	t.Run("assistant/message", func(t *testing.T) {
		_, err := projectTranscript("s", []domain.SessionEvent{
			evWith(2, domain.SessionEventAssistantMessage, map[string]any{
				"turn": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
				"content":       "调工具",
				"tool_calls":    []any{map[string]any{"call_id": "c1", "name": "read_file"}},
				"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
				"model_profile": "fast",
			}),
		})
		if err == nil {
			t.Fatal("缺 step 的 assistant/message（带 tool_calls）没有被拒绝")
		}
		if !strings.Contains(err.Error(), "seq 2") || !strings.Contains(err.Error(), "turn/step") {
			t.Errorf("错误没有指明缺的是 turn/step 以及是哪条事件：%v", err)
		}
	})
}
