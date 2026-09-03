package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// session.max_turn_chars 的文档语义是「每条历史消息最长多少」。SessionTranscript
// 今天只把它施加在 Content 上，assistant 消息的 ToolCalls[].Arguments 一个字节都
// 不计入——而那正是 G3 打开后新进入模型的东西。
//
// 「写入侧已经截到 maxEventPreviewRunes(2000) 了」不足以顶替这个预算，理由与
// SessionTranscript 自己的注释对 tool 消息给出的理由是同一条：预算不该依赖
// 「哪些角色今天碰巧被预先截过」。何况 2000 是**每个 call** 的上限，一条 assistant
// 可以带任意多个 call，累加起来与用户配的那个数没有任何关系。
func TestTheTranscriptPathHonoursMaxTurnCharsForToolCallArguments(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("参", 500)
	lister := &transcriptOnlyLister{transcript: []port.InferenceMessage{
		{
			Role:    port.RoleAssistant,
			Content: "我调三个工具",
			ToolCalls: []domain.ToolCall{
				{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": long}},
				{ID: "c2", Name: "read_file", Arguments: map[string]string{"path": long}},
				{ID: "c3", Name: "read_file", Arguments: map[string]string{"path": long}},
			},
		},
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

	// 先确认夹具真的带着 tool_calls 走完了这条路——否则下面的断言可能因为
	// 「压根没有 arguments」而恒真。
	if len(msgs[0].ToolCalls) != 3 {
		t.Fatalf("消息上有 %d 个 tool call，要 3 个：夹具没走到 arguments 这条路",
			len(msgs[0].ToolCalls))
	}

	for _, call := range msgs[0].ToolCalls {
		got := call.Arguments["path"]
		if strings.Contains(got, long) {
			t.Errorf("call %s 的 arguments 原样进了模型（%d 个字符，配置的上限是 10）："+
				"session.max_turn_chars 对 tool call 参数完全没生效", call.ID, len([]rune(got)))
		}
	}

	// 预算是**每条消息**的，不是每个字段的：Content 与所有 arguments 的内容量
	// 合计不得超过上限。否则一条带 N 个 call 的 assistant 仍能送出 N 倍于用户
	// 所配的量，而「每条消息最长多少」这句话就不再成立。
	//
	// 量的是内容量而不是最终字符串长度：截断记号是额外附加的，Content 那条路
	// 原本就是这个口径（truncateText 把内容截到 maxChars，再接上记号）。
	if total := contentChars(msgs[0]); total > 10 {
		t.Errorf("这条消息 Content + 所有 arguments 的内容合计 %d 个字符，超过配置的上限 10："+
			"预算按字段各算一份，等于把上限乘上了 call 的个数", total)
	}
}

// contentChars 数一条消息里**模型可见内容**的 rune 数，不含截断记号与省略记号。
// 它是这批测试判断「预算有没有被遵守」的口径。
func contentChars(msg port.InferenceMessage) int {
	n := len([]rune(stripMarkers(msg.Content)))
	for _, call := range msg.ToolCalls {
		for _, v := range call.Arguments {
			n += len([]rune(stripMarkers(v)))
		}
	}
	return n
}

// stripMarkers 去掉 truncateText 的硬截断记号与预算耗尽时的省略记号，留下真正
// 送给模型的原文片段。
func stripMarkers(s string) string {
	if i := strings.Index(s, "\n\n────────"); i >= 0 {
		return s[:i]
	}
	if strings.HasPrefix(s, "[参数被完全省略") {
		return ""
	}
	return s
}

// 截断必须留痕，模型才知道自己看到的是半截参数——与 Content 那条路
// （truncateText 留下 OUTPUT HARD-TRUNCATED）保持同一个约定。
func TestTruncatedToolCallArgumentsSayTheyWereTruncated(t *testing.T) {
	t.Parallel()

	lister := &transcriptOnlyLister{transcript: []port.InferenceMessage{
		{
			Role: port.RoleAssistant,
			ToolCalls: []domain.ToolCall{
				{ID: "c1", Name: "write_file", Arguments: map[string]string{
					"content": strings.Repeat("正", 400),
				}},
			},
		},
	}}

	msgs, err := SessionTranscript(context.Background(), lister, config.SessionConfig{
		DefaultRecentTurns: 6,
		MaxTurnChars:       20,
	}, "s1")
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("夹具没走到 arguments 这条路：%d 条消息", len(msgs))
	}

	got := msgs[0].ToolCalls[0].Arguments["content"]
	if !strings.Contains(got, "TRUNCATED") {
		t.Errorf("参数被截了却没留痕，模型不知道自己看到的是半截：%q", got)
	}
}

// 额度分配顺序必须**确定**。
//
// 参数存在 map[string]string 里，而 Go 的 map 遍历顺序是随机的。额度不足以覆盖
// 所有参数时，「谁先花掉额度、谁被省略」若取决于遍历顺序，同一条历史两次投影就会
// 产出不同的字节：provider 的 prompt 缓存全部落空，而「同样的输入得到同样的请求」
// 也不再成立。
//
// 这条测试跑很多轮而不是一轮：不排序时 map 顺序**碰巧**一致的概率随轮数指数下降，
// 一轮的绿说明不了任何事。
func TestArgumentBudgetIsSpentInADeterministicOrder(t *testing.T) {
	t.Parallel()

	// 额度 6：恰好被字典序第一个参数（6 个字符）花光，第二个必然落到省略分支。
	newLister := func() *transcriptOnlyLister {
		return &transcriptOnlyLister{transcript: []port.InferenceMessage{{
			Role: port.RoleAssistant,
			ToolCalls: []domain.ToolCall{{
				ID: "c1", Name: "write_file",
				Arguments: map[string]string{
					"aaa": strings.Repeat("甲", 6),
					"zzz": strings.Repeat("乙", 6),
				},
			}},
		}}}
	}

	var first string
	for round := 0; round < 200; round++ {
		msgs, err := SessionTranscript(context.Background(), newLister(), config.SessionConfig{
			DefaultRecentTurns: 6,
			MaxTurnChars:       6,
		}, "s1")
		if err != nil {
			t.Fatalf("SessionTranscript: %v", err)
		}
		args := msgs[0].ToolCalls[0].Arguments
		got := args["aaa"] + "\x00" + args["zzz"]
		if round == 0 {
			first = got
			// 先确认这一轮真的把额度花完了——否则两个参数都原样返回，
			// 「每轮结果相同」就成了空过。
			if !strings.Contains(args["zzz"], "省略") {
				t.Fatalf("字典序靠后的参数没有落到省略分支，额度没被花完：%q", args["zzz"])
			}
			continue
		}
		if got != first {
			t.Fatalf("第 %d 轮的投影结果与第 1 轮不同：额度分配顺序取决于 map 的随机遍历顺序，"+
				"同一条历史会产出不同的请求字节\n第 1 轮: %q\n本轮:   %q", round+1, first, got)
		}
	}
}

// 没超预算的 arguments 必须一个字节都不动：截断是超限时的处置，不是常态加工。
func TestToolCallArgumentsWithinBudgetAreUntouched(t *testing.T) {
	t.Parallel()

	lister := &transcriptOnlyLister{transcript: []port.InferenceMessage{
		{
			Role: port.RoleAssistant,
			ToolCalls: []domain.ToolCall{
				{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "notes.md"}},
			},
		},
	}}

	msgs, err := SessionTranscript(context.Background(), lister, config.SessionConfig{
		DefaultRecentTurns: 6,
		MaxTurnChars:       6000,
	}, "s1")
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	if got := msgs[0].ToolCalls[0].Arguments["path"]; got != "notes.md" {
		t.Errorf("没超预算的参数被改动了：%q，要 %q", got, "notes.md")
	}
}
