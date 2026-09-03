package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// applyTurnBudget 的契约（这一批测试逐条钉它）：
//
//  1. session.max_turn_chars 约束的是一条消息的**内容量**，Content 与该消息全部
//     arguments 共享同一份额度——不是每个字段各给一份。
//  2. 额度之外，每个被裁的参数仍保留 argumentPeekRunes 个字符的可辨识前缀，外加
//     一个记号。这部分是有意的额外开销，随被裁参数个数线性增长（见 applyTurnBudget
//     的注释：这份预算把无界的那部分压成有界，不是把总字节数封死）。
//  3. 裁剪后若不比原文短就原样放行——记号有几十个字符，把短参数换成更长的说明
//     既不省字节又丢信息。
//  4. 额度分配顺序确定：call 按切片顺序，参数名按字典序。
//
// 每条都做过变异验证。

// budgetContentChars 数一条消息里真正的**原文内容**，剔除裁剪记号。
// 它是判断「预算有没有被遵守」的口径。
func budgetContentChars(msg port.InferenceMessage) int {
	n := len([]rune(stripTrimNote(msg.Content)))
	for _, call := range msg.ToolCalls {
		for _, v := range call.Arguments {
			n += len([]rune(stripTrimNote(v)))
		}
	}
	return n
}

// stripTrimNote 去掉两种裁剪记号（正文用 truncateText 的 footer，参数用
// argumentTrimNote），留下真正送给模型的原文片段。
//
// 记号文本在这里是硬编码的字面量。若生产的记号改了、这里没改，剥离会失效，
// budgetContentChars 就会**多**算 —— 断言随之变红而不是假绿。失败方向是安全的。
func stripTrimNote(s string) string {
	if i := strings.Index(s, "\n\n────────"); i >= 0 {
		return s[:i]
	}
	if i := strings.Index(s, "…[历史裁剪"); i >= 0 {
		return s[:i]
	}
	return s
}

// 契约 1 + 2：预算是每条消息的，不是每个字段的。
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

	const budget = 10
	msgs, err := SessionTranscript(context.Background(), lister, config.SessionConfig{
		DefaultRecentTurns: 6,
		MaxTurnChars:       budget,
	}, "s1")
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("投影出 %d 条消息，要 1 条", len(msgs))
	}
	// 先确认夹具真的带着 tool_calls 走完了这条路——否则下面每条断言都可能因为
	// 「压根没有 arguments」而恒真。
	if len(msgs[0].ToolCalls) != 3 {
		t.Fatalf("消息上有 %d 个 tool call，要 3 个：夹具没走到 arguments 这条路",
			len(msgs[0].ToolCalls))
	}

	for _, call := range msgs[0].ToolCalls {
		if got := call.Arguments["path"]; strings.Contains(got, long) {
			t.Errorf("call %s 的 arguments 原样进了模型（%d 个字符，配置的上限是 %d）："+
				"session.max_turn_chars 对 tool call 参数完全没生效",
				call.ID, len([]rune(got)), budget)
		}
	}

	// 上界 = 额度 + 每个被裁参数的保底前缀。三个参数都会被裁（各 500 字符）。
	//
	// 断言这个精确的上界、而不是一个宽松的量级，是因为「按字段各给一份额度」的
	// 实现会得到 budget×3 + 前缀，正好落在这条线之外。
	want := budget + argumentPeekRunes*3
	if got := budgetContentChars(msgs[0]); got > want {
		t.Errorf("这条消息的原文内容合计 %d 个字符，上界是 %d（额度 %d + 3 个参数各 %d 保底前缀）："+
			"预算按字段各算一份，等于把上限乘上了 call 的个数",
			got, want, budget, argumentPeekRunes)
	}
}

// 契约 2：裁剪必须留痕，且记号要说清「这是历史裁剪、不是你当时发出的参数、别重试」。
//
// 记号不能沿用 truncateText 给工具**输出**写的那段 footer：它自称「输出被硬截断」
// 「非数据或参数问题」，而被裁的恰恰是参数；嵌在 write_file 的 content 里还会被
// 模型读成文件正文的一部分。
func TestTrimmedToolCallArgumentsSayTheyWereTrimmedByHistory(t *testing.T) {
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
	if !strings.Contains(got, "HISTORY-TRIMMED") {
		t.Errorf("参数被裁了却没留痕，模型不知道自己看到的是半截：%q", got)
	}
	if !strings.Contains(got, "请勿据此重试") {
		t.Errorf("记号没告诉模型别据此重试——那正是 60 次重复调用那场事故的成因：%q", got)
	}
	if strings.Contains(got, "OUTPUT HARD-TRUNCATED") {
		t.Errorf("参数位置用了给工具**输出**写的 footer，它自称「非参数问题」而被裁的正是参数：%q", got)
	}
}

// 契约 3：裁剪后若不比原文短，就原样放行。
//
// 记号有几十个字符。把 5 个字符的 {"path":"a.md"} 换成一段更长的说明，既没省下
// 字节（反而多出十几倍），又把模型唯一能用的信息换掉了。短参数不是这份预算的敌人。
func TestAShortArgumentIsNotReplacedByALongerNote(t *testing.T) {
	t.Parallel()

	const short = "a.md"
	lister := &transcriptOnlyLister{transcript: []port.InferenceMessage{
		{
			Role: port.RoleAssistant,
			// Content 先把额度吃光，于是后面的短参数落到「额度已尽」那条路上。
			Content: strings.Repeat("文", 50),
			ToolCalls: []domain.ToolCall{
				{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": short}},
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
	// 先确认额度确实被 Content 吃光了——否则这个参数是因为「还有额度」才原样
	// 通过的，这条测试就没在验它该验的东西。
	if !strings.Contains(msgs[0].Content, "TRUNCATED") {
		t.Fatalf("Content 没被截，额度没被吃光，这条测试验不到「额度已尽」那条路：%q",
			msgs[0].Content)
	}

	got := msgs[0].ToolCalls[0].Arguments["path"]
	if got != short {
		t.Errorf("短参数被改成了 %q（%d 字符），原文只有 %d 字符："+
			"换上去比原文还长，既没省字节又丢了模型唯一能用的信息",
			got, len([]rune(got)), len([]rune(short)))
	}
}

// 契约 3 的另一半：**中等长度**的参数也不能被换成更长的东西。
//
// 上一条用的是 4 个字符的短参数，它在「保底前缀已经覆盖全文」那一步就原样返回了，
// 根本走不到「换上去比原文还长就不换」那个守卫——删掉那个守卫，上一条测试照样绿。
// 真正落在守卫上的是长度介于「保底前缀」与「保底前缀 + 记号」之间的参数：裁剪后
// 反而更长的，正是这一段。
func TestAMediumArgumentIsNotReplacedByALongerNote(t *testing.T) {
	t.Parallel()

	// 50 字符：比保底前缀（8）长，但远短于「8 + 记号」，裁剪后一定更长。
	medium := strings.Repeat("中", 50)
	lister := &transcriptOnlyLister{transcript: []port.InferenceMessage{
		{
			Role:    port.RoleAssistant,
			Content: strings.Repeat("文", 50), // 先把额度吃光
			ToolCalls: []domain.ToolCall{
				{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": medium}},
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
	if !strings.Contains(msgs[0].Content, "TRUNCATED") {
		t.Fatalf("Content 没被截，额度没被吃光，验不到「额度已尽」那条路：%q", msgs[0].Content)
	}

	got := msgs[0].ToolCalls[0].Arguments["path"]
	if n := len([]rune(got)); n > len([]rune(medium)) {
		t.Errorf("参数从 %d 字符被「裁」成了 %d 字符——裁剪反而变长了，"+
			"既没省字节又丢了原文：%q", len([]rune(medium)), n, got)
	}
}

// 契约 1 的另一半：裁剪路径**也**要扣额度。
//
// 上面那条「预算是每条消息的」用的是超长参数（500 字符），它们全都落到保底前缀上，
// 于是扣不扣额度结果都一样——删掉裁剪路径的扣减，那条测试照样绿。要区分，得让
// 剩余额度落在「保底前缀」与「参数全长」之间：第一个参数吃掉一大截额度后，第二个
// 才拿得到明显更少的份额。
func TestTrimmingAlsoSpendsTheBudget(t *testing.T) {
	t.Parallel()

	// 额度 30，两个各 200 字符的参数：aaa 裁到 30 并把额度扣光，zzz 只剩保底前缀。
	// 若裁剪路径不扣额度，zzz 会和 aaa 一样拿到 30。
	long := strings.Repeat("甲", 200)
	lister := &transcriptOnlyLister{transcript: []port.InferenceMessage{{
		Role: port.RoleAssistant,
		ToolCalls: []domain.ToolCall{{
			ID: "c1", Name: "write_file",
			Arguments: map[string]string{"aaa": long, "zzz": strings.Repeat("乙", 200)},
		}},
	}}}

	msgs, err := SessionTranscript(context.Background(), lister, config.SessionConfig{
		DefaultRecentTurns: 6,
		MaxTurnChars:       30,
	}, "s1")
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	args := msgs[0].ToolCalls[0].Arguments
	aaaKept := len([]rune(stripTrimNote(args["aaa"])))
	zzzKept := len([]rune(stripTrimNote(args["zzz"])))
	if aaaKept != 30 {
		t.Fatalf("aaa 保留了 %d 个字符，要 30（整个额度）：夹具没落在该验的那段上", aaaKept)
	}
	if zzzKept != argumentPeekRunes {
		t.Errorf("zzz 保留了 %d 个字符，要 %d（保底前缀）：裁剪路径没扣额度，"+
			"每个参数都拿到了完整的一份，等于把上限乘上了参数个数", zzzKept, argumentPeekRunes)
	}
}

// 契约 4：额度分配顺序必须确定。
//
// 参数存在 map[string]string 里，而 Go 的 map 遍历顺序随机。额度不足以覆盖所有
// 参数时，「谁先花掉额度、谁只剩保底前缀」若取决于遍历顺序，同一条历史两次投影
// 就会产出不同的字节，provider 的 prompt 缓存全部落空。
//
// 注意 sort 钉住的**不是** wire 上的 key 顺序——json.Marshal(map[string]string)
// 本来就按 key 排序。它钉住的是「哪个参数被裁」。
func TestArgumentBudgetIsSpentInADeterministicOrder(t *testing.T) {
	t.Parallel()

	// 额度 30 恰好被字典序第一个参数吃光，第二个只剩保底前缀 + 记号。
	// 两个参数都远长于记号，所以都不会走「原样放行」那条路。
	aaa := strings.Repeat("甲", 30)
	zzz := strings.Repeat("乙", 300)
	newLister := func() *transcriptOnlyLister {
		return &transcriptOnlyLister{transcript: []port.InferenceMessage{{
			Role: port.RoleAssistant,
			ToolCalls: []domain.ToolCall{{
				ID: "c1", Name: "write_file",
				Arguments: map[string]string{"aaa": aaa, "zzz": zzz},
			}},
		}}}
	}

	var first string
	for round := 0; round < 200; round++ {
		msgs, err := SessionTranscript(context.Background(), newLister(), config.SessionConfig{
			DefaultRecentTurns: 6,
			MaxTurnChars:       30,
		}, "s1")
		if err != nil {
			t.Fatalf("SessionTranscript: %v", err)
		}
		args := msgs[0].ToolCalls[0].Arguments
		got := args["aaa"] + "\x00" + args["zzz"]
		if round == 0 {
			first = got
			// 直接钉住「是字典序靠前的那个活下来」，而不是绕道从「zzz 被裁」
			// 反推：降序排序同样能让每轮结果一致，只断言「各轮相同」放得过它。
			if args["aaa"] != aaa {
				t.Fatalf("字典序靠前的 aaa 没有拿到额度（%q）：额度不是按字典序分配的", args["aaa"])
			}
			// 也确认额度真的被花完了，否则两个参数都原样返回，
			// 「每轮结果相同」就成了空过。
			if !strings.Contains(args["zzz"], "HISTORY-TRIMMED") {
				t.Fatalf("字典序靠后的 zzz 没有被裁，额度没被花完：%q", args["zzz"])
			}
			continue
		}
		if got != first {
			t.Fatalf("第 %d 轮的投影结果与第 1 轮不同：额度分配顺序取决于 map 的随机遍历顺序，"+
				"同一条历史会产出不同的请求字节\n第 1 轮: %q\n本轮:   %q", round+1, first, got)
		}
	}
}

// 已知代价，显式钉住：被裁的历史参数不再与本轮的真实调用逐字相等，
// 于是跨会话的重复调用在 repeatedCallStreak 那里漏检。
//
// 链路：runtime.go 的 appendHistory 把历史塞进 convo.messages → repeatedCallStreak
// 遍历它 → callsKey → dedupKey 按 `name|k=v` **逐字**比对。历史里被裁过的参数与
// 模型这一轮真实发出的参数不等，streak 断在那里。
//
// 这不是这次改动**引入**的问题：写入侧本来就把 arguments 截到 maxEventPreviewRunes
// (2000)，超过那个长度的参数改动前同样匹配不上。这次把不匹配的边界从「超过 2000」
// 拉到了「超过剩余额度」，范围扩大了。
//
// 保持现状而不是修它，是因为「跨会话的同一组调用算不算连续重复」本身是一个未定的
// 设计问题，不该在一个预算改动里顺手决定。这条测试的作用是让这个取舍**可见**：
// 谁哪天改了裁剪或改了 dedupKey，这里会告诉他这条链路存在。
func TestTrimmedHistoryArgumentsNoLongerMatchTheLiveCallVerbatim(t *testing.T) {
	t.Parallel()

	original := strings.Repeat("甲", 300)
	live := domain.ToolCall{ID: "live", Name: "write_file",
		Arguments: map[string]string{"content": original}}

	lister := &transcriptOnlyLister{transcript: []port.InferenceMessage{{
		Role: port.RoleAssistant,
		ToolCalls: []domain.ToolCall{
			{ID: "old", Name: "write_file", Arguments: map[string]string{"content": original}},
		},
	}}}
	msgs, err := SessionTranscript(context.Background(), lister, config.SessionConfig{
		DefaultRecentTurns: 6,
		MaxTurnChars:       20,
	}, "s1")
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	historical := msgs[0].ToolCalls[0]

	// 先确认这一轮真的裁了——没裁的话两边本来就相等，下面的断言验不到东西。
	if !strings.Contains(historical.Arguments["content"], "HISTORY-TRIMMED") {
		t.Fatalf("历史参数没被裁，这条测试验不到该验的链路：%q", historical.Arguments["content"])
	}

	if dedupKey(historical) == dedupKey(live) {
		t.Log("裁剪后的历史参数仍与本轮调用逐字相等——" +
			"若这是有意的修复（例如 dedupKey 改成对裁剪不敏感），请连同上面的注释一起更新")
		return
	}
	// 这是当前的、已知的行为。断言它，而不是断言「相等」，是为了如实记录：
	// 跨会话的这一组重复调用在今天的实现里漏检。
	if streak := repeatedCallStreak(msgs, []domain.ToolCall{live}); streak != 1 {
		t.Errorf("streak = %d，当前实现下应为 1（历史参数被裁，逐字比对断开）："+
			"若这里变了，说明 dedupKey 或裁剪的行为改了，注释里那个取舍需要重新评估", streak)
	}
}

// 没超预算的 arguments 必须一个字节都不动：裁剪是超限时的处置，不是常态加工。
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

// 裁剪只影响这一次出站请求，不得改到调用方手里的那份历史。
//
// applyTurnBudget 收到的 msg 是元素的值拷贝，但 ToolCalls 的切片头指向调用方那块
// 底层数组——只换 map 不够，切片本身也要复制。今天生产上 projectTranscript 每次
// 都新构造所以看不出来，但那是没写进 ConversationTurnLister 契约的前提：一个带
// 缓存的 lister 会让第二次投影读到第一次裁剪过的参数（记号被再裁一次、再接一个
// 记号）。
func TestTrimmingDoesNotWriteThroughToTheCallersSlice(t *testing.T) {
	t.Parallel()

	original := strings.Repeat("原", 300)
	shared := []port.InferenceMessage{{
		Role: port.RoleAssistant,
		ToolCalls: []domain.ToolCall{
			{ID: "c1", Name: "write_file", Arguments: map[string]string{"content": original}},
		},
	}}
	// 夹具对消息切片做的是**浅**拷贝（transcriptOnlyLister），所以 ToolCalls 的
	// 底层数组是共享的——这正是生产上一个带缓存的 lister 会有的形状。
	lister := &transcriptOnlyLister{transcript: shared}

	msgs, err := SessionTranscript(context.Background(), lister, config.SessionConfig{
		DefaultRecentTurns: 6,
		MaxTurnChars:       20,
	}, "s1")
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	if got := msgs[0].ToolCalls[0].Arguments["content"]; !strings.Contains(got, "HISTORY-TRIMMED") {
		t.Fatalf("这一轮没有发生裁剪，写穿与否验不出来：%q", got)
	}
	if got := shared[0].ToolCalls[0].Arguments["content"]; got != original {
		t.Errorf("调用方持有的那份参数被改写了（%d 字符，原文 %d 字符）："+
			"裁剪写穿了共享的切片底层数组，下一次投影会在记号上再裁一次",
			len([]rune(got)), len([]rune(original)))
	}
}
