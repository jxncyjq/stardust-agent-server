package runtime

import (
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// repeatedCallStreak 数的是「连续多少**轮**请求了完全相同的工具调用」，而这里的
// 「轮」是**这一个任务内**工具循环的轮次——它要抓的是「模型卡在循环里」。
//
// G3 打开后历史以 transcript 排进了同一个 conversation，于是从后往前的扫描会一路
// 扫进**上一个会话**的 assistant 消息。后果不是漏检，是误报：
//
//   - 用户连问三次同样的问题（完全正常的用法），下一个任务的**第一次**模型请求
//     streak 就是 4，超过 repeatWarnStreak(3)，模型平白收到一条「你在重复调用」的
//     警告——它才刚开始，一次工具都还没调；
//   - 历史里连续 7 轮同样的调用，streak 到 8，撞上 repeatAbortStreak，任务**直接
//     被中止**。
//
// 熔断可以少管（漏检只是没救到），但不能错杀。所以历史必须排除在 streak 之外。
//
// 注：MIN-4 的参数裁剪会让长参数的历史调用与本轮不再逐字相等，从而**意外地**避开
// 这个误报——但那只对超过预算的长参数有效，路径、开关这类短参数照样原样进历史，
// 误报照旧。别把那个副作用当成防线。
func TestHistoryToolCallsDoNotCountIntoTheRepeatStreak(t *testing.T) {
	t.Parallel()

	live := []domain.ToolCall{{ID: "now", Name: "read_file",
		Arguments: map[string]string{"path": "config.md"}}}
	// 一模一样的调用，短参数，不会被 MIN-4 的预算裁到。
	historical := domain.ToolCall{ID: "old", Name: "read_file",
		Arguments: map[string]string{"path": "config.md"}}

	convo := newConversation("base prompt", nil)
	// 历史里连续 3 轮都是同一个调用：用户连问了三次同样的问题。
	var history []port.InferenceMessage
	for i := 0; i < 3; i++ {
		history = append(history,
			port.InferenceMessage{Role: port.RoleAssistant, Content: "我读一下",
				ToolCalls: []domain.ToolCall{historical}},
			port.InferenceMessage{Role: port.RoleTool, ToolCallID: "old", Content: "文件内容"})
	}
	convo.appendHistory(history)
	convo.appendCurrentInput("再读一次 config.md")

	// 本任务一次工具都还没跑过，所以这是「第一轮」——streak 必须是 1。
	if got := convo.repeatedCallStreak(live); got != 1 {
		t.Errorf("streak = %d，要 1：本任务还没跑过任何一轮，历史里那三轮被算进来了。"+
			"warn=%d abort=%d —— 这个数已经越过警告线，模型会平白收到重复调用警告，"+
			"历史再长一点就会被直接中止", got, repeatWarnStreak, repeatAbortStreak)
	}
}

// 排除历史不能把**本任务内**的重复也一并排除掉——那会让熔断彻底失效。
func TestTheRepeatStreakStillCountsRoundsWithinTheTask(t *testing.T) {
	t.Parallel()

	call := domain.ToolCall{ID: "c", Name: "read_file",
		Arguments: map[string]string{"path": "config.md"}}
	live := []domain.ToolCall{call}

	convo := newConversation("base prompt", nil)
	convo.appendHistory([]port.InferenceMessage{
		{Role: port.RoleAssistant, Content: "历史里的别的事", ToolCalls: []domain.ToolCall{
			{ID: "old", Name: "list_files", Arguments: map[string]string{"path": "."}}}},
		{Role: port.RoleTool, ToolCallID: "old", Content: "..."},
	})
	convo.appendCurrentInput("读 config.md")

	// 本任务内连续两轮请求了同样的调用。
	convo.appendAssistant("我读一下", []domain.ToolCall{call})
	convo.appendToolResults([]domain.ToolCall{call}, map[string]string{"c": "文件内容"})
	convo.appendAssistant("再读一次", []domain.ToolCall{call})
	convo.appendToolResults([]domain.ToolCall{call}, map[string]string{"c": "文件内容"})

	// 两轮已记录 + 待发的这一轮 = 3。
	if got := convo.repeatedCallStreak(live); got != 3 {
		t.Errorf("streak = %d，要 3：本任务内的连续重复必须照数，"+
			"排除历史不能顺手把熔断本身关掉", got)
	}
}

// 关闭 G3 时没有历史，行为必须与改动前逐字一致。
func TestTheRepeatStreakIsUnchangedWithoutHistory(t *testing.T) {
	t.Parallel()

	call := domain.ToolCall{ID: "c", Name: "read_file",
		Arguments: map[string]string{"path": "a.md"}}
	live := []domain.ToolCall{call}

	convo := newConversation("base prompt", nil)
	convo.appendAssistant("读", []domain.ToolCall{call})
	convo.appendToolResults([]domain.ToolCall{call}, map[string]string{"c": "内容"})

	if got := convo.repeatedCallStreak(live); got != 2 {
		t.Errorf("streak = %d，要 2（一轮已记录 + 待发的这一轮）：G3 关闭时的行为变了", got)
	}
}
