package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// defaultRecentTurns and defaultMaxTurnChars mirror the CLI session defaults
// (normalizeRecentTurns / normalizeMaxTurnChars): a configured 0 means "unset",
// not "inject nothing" or "never truncate".
const (
	defaultRecentTurns  = 6
	defaultMaxTurnChars = 6000
)

// transcriptMessagesPerTurn converts the history budget from the unit the
// text-block path budgets in (DefaultRecentTurns conversation turns) into the
// unit the transcript path needs (messages).
//
// One conversation turn becomes one user message, or — for an assistant turn —
// one assistant message plus one tool message per call it made. The factor is a
// deliberate over-estimate: G3 exists so the model can see those tool
// round-trips, and a budget that clipped them away would defeat it. It is
// nonetheless a cap, because reading a whole session back would let one long
// conversation grow every request without bound, and G3's cost (spec §3) is
// request size — the one thing it must keep measurable.
const transcriptMessagesPerTurn = 8

// SessionHistory is one task's session history in whichever of the two shapes
// G3 selects. Exactly one of the fields is ever populated (both are empty when
// there is no history to inject), which is why they are returned together
// rather than as two independent loads: a caller cannot end up injecting both
// and sending the same history twice.
type SessionHistory struct {
	// Turns is the G3-off shape: the turns the cognitive Core renders into the
	// prompt's "Recent conversation:" block.
	Turns []domain.ConversationTurn
	// Transcript is the G3-on shape: provider messages appended after
	// message[0], carrying the history's tool round-trips.
	Transcript []port.InferenceMessage
}

// SessionHistoryForTask loads task's session history in the shape
// sessionCfg.ToolTranscriptEnabled selects. It is the ONE place that selection
// is made for a runner that has a domain.Task, and there are exactly two of
// those: AgentRuntimeResolver (per-agent runtimes) and the CLI's
// defaultTaskRunner (every task whose AgentID is absent from the agent registry,
// the path the GUI's default agent takes).
//
// There is a THIRD production path that reads session history and cannot call
// this function: `legion tui` loads it from a session id, before any
// domain.Task exists (tuiSessionController.SessionHistory, which makes the same
// selection and shares SessionTranscript for the transcript half). It was the
// path this switch originally missed — the switch worked under serve and did
// nothing under tui, with no error anywhere.
//
// That shape — the seam is here but a caller silently stays on the old path —
// is how the GUI once ended up with no cross-turn memory at all, so every one
// of the three call sites is pinned by an assertion on what the model actually
// received: TestDefaultTaskRunnerSendsHistoryAsATranscript (internal/cli),
// TestTheResolverSendsHistoryAsATranscript (this package) and
// TestTheTUIPathSendsHistoryAsATranscript (internal/cli). Deleting any one
// assignment turns exactly one of them red.
//
// Switch off (the default, spec §3: G3 "不该在做轨迹的顺路上悄悄打开") is the
// existing path unchanged, byte for byte: RecentTurnsForTask, and the history
// stays inside the prompt text.
//
// A nil lister or a task with no SessionID is a legitimate "no session history
// configured" state and yields an empty SessionHistory. A lister error is
// returned wrapped: failing to read history must not be rendered as "there is
// none".
func SessionHistoryForTask(ctx context.Context, lister ConversationTurnLister, sessionCfg config.SessionConfig, task domain.Task) (SessionHistory, error) {
	if !sessionCfg.ToolTranscriptEnabled {
		turns, err := RecentTurnsForTask(ctx, lister, sessionCfg, task)
		if err != nil {
			return SessionHistory{}, err
		}
		return SessionHistory{Turns: turns}, nil
	}
	if lister == nil || strings.TrimSpace(task.SessionID) == "" {
		return SessionHistory{}, nil
	}
	// The task's own user turn needs no filtering here, the way RecentTurnsForTask
	// drops it by TaskID: this call happens BEFORE the task runs, and both writers
	// of a user/message event record it later — RunTask's own recorder (opened
	// further down RunTask) and the TUI controller's RecordExchange (which writes
	// the question together with the answer, after the exchange). The projected
	// messages carry no task_id anyway, so filtering here is not available; if a
	// third writer ever records a task's question up front, this is where the
	// duplicate would show up.
	msgs, err := SessionTranscript(ctx, lister, sessionCfg, task.SessionID)
	if err != nil {
		return SessionHistory{}, err
	}
	return SessionHistory{Transcript: msgs}, nil
}

// SessionTranscript loads sessionID's history in G3's transcript shape and
// applies BOTH budgets the session config declares, so the switch changes the
// history's SHAPE and not the limits a deployment configured.
//
// It exists as its own exported function because the transcript branch has two
// callers that cannot share SessionHistoryForTask: the serve-side runners have a
// domain.Task, the TUI's session controller has only a session id (see
// tuiSessionController.SessionHistory). Duplicating the budget arithmetic in the
// TUI is how one path silently ends up unbounded.
//
//   - Session.DefaultRecentTurns caps how MANY messages come back, converted into
//     the transcript's unit by transcriptMessagesPerTurn.
//   - Session.MaxTurnChars caps how LONG each one is. Without this the two paths
//     disagree by two orders of magnitude: RecentTurnsForTask truncates every turn
//     at MaxTurnChars (6000 by default), while the projection passes user/assistant
//     content through verbatim and those events are documented to store the FULL
//     text, bounded only by P1's 64 KiB per event. A configured
//     session.max_turn_chars would then silently do nothing the moment G3 is
//     turned on — and G3's whole cost (spec §3) is request size, "the one thing it
//     must keep measurable". Tool messages are already bounded on the write side
//     (maxEventPreviewRunes = 2000), so in practice this only bites user/assistant
//     bodies; it is applied uniformly anyway rather than by role, because "which
//     roles happen to be pre-truncated today" is not something this budget should
//     depend on. The same reasoning is why it covers an assistant's
//     ToolCalls[].Arguments too, and why the budget is per MESSAGE rather than per
//     field — see applyTurnBudget.
//
// A nil lister or an empty sessionID is the legitimate "no session history
// configured" state and yields (nil, nil). A lister error is wrapped and
// returned: failing to read history must never be rendered as "there is none".
// 守卫：TestTheTranscriptPathHonoursMaxTurnChars。
func SessionTranscript(ctx context.Context, lister ConversationTurnLister, sessionCfg config.SessionConfig, sessionID string) ([]port.InferenceMessage, error) {
	if lister == nil || strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	limit := sessionCfg.DefaultRecentTurns
	if limit <= 0 {
		limit = defaultRecentTurns
	}
	// truncateText treats maxChars <= 0 as "no truncation", so an explicitly zero
	// MaxTurnChars would inject unbounded bodies here — the same normalization
	// RecentTurnsForTask does, for the same reason.
	maxTurnChars := sessionCfg.MaxTurnChars
	if maxTurnChars <= 0 {
		maxTurnChars = defaultMaxTurnChars
	}
	msgs, err := lister.ListConversationTranscript(ctx, sessionID, limit*transcriptMessagesPerTurn)
	if err != nil {
		return nil, fmt.Errorf("list conversation transcript for session %q: %w", sessionID, err)
	}
	for i := range msgs {
		msgs[i] = applyTurnBudget(msgs[i], maxTurnChars)
	}
	return msgs, nil
}

// applyTurnBudget 把 maxTurnChars 施加在一条历史消息的**全部**模型可见内容上：
// Content 与 assistant 的 ToolCalls[].Arguments 共享同一份额度，先来后到。
//
// 为什么 arguments 也要算进来：max_turn_chars 的语义是「每条历史消息最长多少」，
// 而 G3 打开后新进入模型的恰恰就是 tool call 的参数。只截 Content 的话，一条带
// N 个 call 的 assistant 能送出 N 份不受任何约束的参数，用户配的那个数就不再是
// 这条消息的上限。
//
// 「写入侧已经按 maxEventPreviewRunes(2000) 截过了」不足以顶替这份预算，理由与
// 上面对 tool 消息给出的是同一条：预算不该依赖「哪些角色今天碰巧被预先截过」。
// 何况 2000 是**每个 call** 的上限，累加起来与配置值没有关系。
//
// 额度只计**内容**，不计截断记号：这与 Content 那条路原本的口径一致
// （truncateText 把内容截到 maxChars，记号是额外附加的）。
//
// 所以这份预算收紧的是内容量，不是最终字节数：一条消息的实际上限是
// maxTurnChars + 一个 262 字符的正文 footer + 每个被裁参数一个 ~100 字符的记号，
// 而参数个数由模型每轮发几个 call 决定，不受这份预算约束。说「总量受控」是不对的；
// 它把无界的那部分（参数内容）压成了有界，剩下的是随 call 个数线性增长的记号。
//
// 遍历顺序必须确定：call 按切片顺序，参数名按字典序。map 的随机遍历顺序会让同一
// 条历史两次投影出不同的字节，既毁掉 provider 的 prompt 缓存，也让「同样的输入
// 得到同样的请求」不再成立。（排序钉住的不是 wire 上的 key 顺序——
// json.Marshal(map[string]string) 本来就按 key 排序——而是「哪个参数被裁」。）
//
// 已知代价：被裁的历史参数不再与本轮真实调用逐字相等，于是 repeatedCallStreak →
// callsKey → dedupKey 那条逐字比对的链路会漏掉跨会话的重复调用。写入侧本就把
// arguments 截到 maxEventPreviewRunes(2000)，所以这不是新问题，只是把不匹配的
// 边界从「超过 2000」拉到了「超过剩余额度」。「跨会话的同一组调用算不算连续重复」
// 是一个未定的设计问题，不在一个预算改动里顺手决定。
// 钉住取舍：TestTrimmedHistoryArgumentsNoLongerMatchTheLiveCallVerbatim。
// 守卫：TestTheTranscriptPathHonoursMaxTurnCharsForToolCallArguments。
func applyTurnBudget(msg port.InferenceMessage, maxTurnChars int) port.InferenceMessage {
	remaining := maxTurnChars

	// spendContent 从剩余额度里扣掉 s 实际占用的内容量，返回该发给模型的文本。
	// 它用于消息正文，截断记号沿用 truncateText 的那一套。
	spendContent := func(s string) string {
		if s == "" {
			return s
		}
		out := truncateText(s, remaining)
		spendBudget(&remaining, s)
		return out
	}

	msg.Content = spendContent(msg.Content)
	// 复制切片再改：msg 是调用方那条消息的**值**拷贝，但 msg.ToolCalls 的切片头
	// 指向同一块底层数组，就地写 msg.ToolCalls[i] 会改到调用方手里的那份。今天
	// 生产上 projectTranscript 每次都新构造所以看不出来，但那是没写进
	// ConversationTurnLister 契约的前提——一个带缓存的 lister 会让第二次投影读到
	// 第一次裁剪过的参数（记号被再裁一次、再接一个记号），且不会有任何测试变红。
	msg.ToolCalls = append([]domain.ToolCall(nil), msg.ToolCalls...)
	for i := range msg.ToolCalls {
		args := msg.ToolCalls[i].Arguments
		if len(args) == 0 {
			continue
		}
		names := make([]string, 0, len(args))
		for name := range args {
			names = append(names, name)
		}
		sort.Strings(names)
		// 就地改写会污染调用方持有的那份 map，所以换一份新的。切片也已在上面
		// 整个复制过了——只换 map 不够：msg 是元素的值拷贝，msg.ToolCalls 的
		// 切片头仍指向调用方那块底层数组。
		trimmed := make(map[string]string, len(args))
		for _, name := range names {
			trimmed[name] = spendArgument(&remaining, args[name])
		}
		msg.ToolCalls[i].Arguments = trimmed
	}
	return msg
}

// spendBudget 从 remaining 里扣掉 s 实际占用的内容量（额度只计内容，不计记号）。
func spendBudget(remaining *int, s string) {
	if n := len([]rune(s)); n < *remaining {
		*remaining -= n
	} else {
		*remaining = 0
	}
}

// argumentTrimNote 是参数被历史投影裁剪时留下的记号。
//
// 它不能复用 truncateText 的 footer。那段文字是给工具**输出**写的，原话是
// 「输出被硬截断……非数据或参数问题——换参数或换工具重试不会有帮助」：
//
//   - 用在参数位置上它自称「输出」、自称「非参数问题」，而被裁的恰恰是参数；
//   - 它嵌在 write_file 的 content 参数里时，那 262 个字符极可能被模型读成
//     「我当时写进文件的内容的一部分」。
//
// footer 存在的理由本是「别让模型把截断读成参数错误后换参数重试」（那次 60 次
// 调用的事故），搬到参数位置反而**制造**那个失败模式。所以这里自己说三件事：
// 这是历史渲染的裁剪、你当时实际发出的不是这个、不要据此重试。
func argumentTrimNote(kept, total int) string {
	return fmt.Sprintf(
		"…[历史裁剪 / HISTORY-TRIMMED：仅存前 %d / 共 %d 字符。"+
			"这是会话历史为省上下文做的裁剪，不是你当时实际发出的参数，也不表示调用失败——请勿据此重试]",
		kept, total)
}

// argumentPeekRunes 是额度耗尽后仍为每个参数保留的可辨识前缀长度。
//
// 留一点而不是一个字不留：参数是「这次调用在干什么」的唯一载体——紧随其后的
// tool 消息只带 call_id 与结果正文，既没有工具名也没有参数。参数整段消失会让
// 那份结果在模型眼里变成「某次不知道在干嘛的调用返回了这些」，等于连带废掉了
// 后面那条消息。
const argumentPeekRunes = 8

// spendArgument 按剩余额度裁剪一个 tool call 的参数值，并扣减额度。
//
// 额度耗尽时不能回落到 truncateText(s, 0)——它把非正上限解释成「不截断」，
// 会把整段原文原样放行，正是这份预算要堵的洞。
//
// 裁剪后若不比原文短，就原样返回：记号本身有几十个字符，把
// `{"path":"a.md"}` 换成一段更长的说明，既没省下字节又丢掉了模型唯一能用的
// 信息。短参数不是这份预算的敌人。
func spendArgument(remaining *int, s string) string {
	if s == "" {
		return s
	}
	total := len([]rune(s))
	keep := *remaining
	if keep > total {
		keep = total
	}
	if keep >= total {
		spendBudget(remaining, s)
		return s
	}
	if keep < argumentPeekRunes {
		keep = argumentPeekRunes
	}
	if keep >= total {
		spendBudget(remaining, s)
		return s
	}
	out := string([]rune(s)[:keep]) + argumentTrimNote(keep, total)
	if len([]rune(out)) >= total {
		// 换上去比原文还长：不换。
		spendBudget(remaining, s)
		return s
	}
	spendBudget(remaining, string([]rune(s)[:keep]))
	return out
}

// RecentTurnsForTask loads the conversation turns to inject as cross-turn
// memory for task, so a follow-up question can see what the session already
// said. It is the G3-OFF half of SessionHistoryForTask, which is what the two
// runners holding a domain.Task call — AgentRuntimeResolver (per-agent
// runtimes) and the CLI's defaultTaskRunner (every task whose AgentID is absent
// from the agent registry, the path the GUI's default agent takes). `legion tui`
// is the third history-reading path and goes through
// tuiSessionController.SessionHistory instead, because it has only a session id;
// see SessionHistoryForTask for the full list and what pins each one.
//
// Wiring history into only one of these paths is what left the GUI with no
// cross-turn memory at all, so a new runner must go through SessionHistoryForTask
// (or, without a task, SessionTranscript) rather than re-deriving the rules below.
//
// It asks for one more turn than the limit and then drops the task's own user
// turn: the HTTP layer persists that turn (as "<taskID>:user") before the task
// is enqueued, so it would otherwise be replayed alongside task.Input.
//
// A nil lister or a task with no SessionID is a legitimate "no session history
// configured" state and yields (nil, nil). A lister error is returned wrapped:
// failing to read history must not be silently rendered as "there is none".
func RecentTurnsForTask(ctx context.Context, lister ConversationTurnLister, sessionCfg config.SessionConfig, task domain.Task) ([]domain.ConversationTurn, error) {
	if lister == nil || strings.TrimSpace(task.SessionID) == "" {
		return nil, nil
	}
	limit := sessionCfg.DefaultRecentTurns
	if limit <= 0 {
		limit = defaultRecentTurns
	}
	// truncateText treats maxChars <= 0 as "no truncation", so an explicitly
	// zero MaxTurnChars would inject unbounded turn bodies here while the CLI
	// path capped them at the default.
	maxTurnChars := sessionCfg.MaxTurnChars
	if maxTurnChars <= 0 {
		maxTurnChars = defaultMaxTurnChars
	}
	// +1: the task's own user turn is already persisted and will be filtered out.
	turns, err := lister.ListConversationTurns(ctx, task.SessionID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("list conversation turns for session %q: %w", task.SessionID, err)
	}
	out := make([]domain.ConversationTurn, 0, len(turns))
	for _, turn := range turns {
		if turn.TaskID == task.ID {
			continue
		}
		turn.Content = truncateText(turn.Content, maxTurnChars)
		out = append(out, turn)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}
