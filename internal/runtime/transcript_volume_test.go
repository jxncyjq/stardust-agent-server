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

// 体积夹具里的标记。它们让「历史真的到了模型面前」与「有个长得像的东西到了」
// 分得开，也让下面的断言能指名道姓地说出差值是被谁撑起来的。
const (
	volumeSessionID   = "sess-volume"
	volumeToolMarker  = "VOLUME-TOOL-OUTPUT"
	volumeUserMarker  = "VOLUME-USER"
	volumeSpillPath   = ".legion/spill/vol-1.txt"
	volumeContextFile = "项目约定：先读再改。"
)

// volumeToolPreview 是一次工具往返在事件日志里留下的**预览**（写入侧按
// maxEventPreviewRunes 截过，这里模拟一份没到那个上限的真实大小）。
//
// 它有几百个字符而不是十几个，是因为 G3 的代价就落在这里：关闭时这段内容**根本
// 不进模型**（turns 视图只有「谁说了什么」），打开时它整段进。用一段十几字的假
// 输出来量这个代价，量出来的会是一个和真实部署无关的数字。
func volumeToolPreview(tag string) string {
	return volumeToolMarker + " " + tag + "\n" +
		strings.Repeat("缓存命中率随分片数上升而下降，热点键集中在前 3 个分片。\n", 8)
}

// seedVolumeHistorySession 写入两轮**已完成的**历史，每轮都带真实的工具往返：
// 提问 → assistant 宣告调用 → 工具结果 → 收尾回答。
//
// 两轮而不是一轮，是因为体积的差值要能反映「历史越长、代价越大」这件事；只有
// 一轮时差值仍然为正，但看不出它随历史增长。
func seedVolumeHistorySession(t *testing.T, ctx context.Context, repo *storage.SQLiteRepository, sessionID string) {
	t.Helper()
	at := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	var events []domain.SessionEvent
	seq := int64(0)
	next := func(typ domain.SessionEventType, payload map[string]any) {
		events = append(events, projectionEvent(t, seq, typ, at.Add(time.Duration(seq)*time.Second), payload))
		seq++
	}
	for turn := 0; turn < 2; turn++ {
		nth := string(rune('1' + turn))
		callID := "vol-c" + nth
		// 每一轮是**一个自己的任务**，所以 task_id 每轮不同。这不是随手取的名字：
		// projectTurns 把 turn 折叠在 task_id+role 这个键上，后者覆盖前者
		// （internal/storage/project_turns.go 的 fold），两轮共用一个 task_id 会让
		// 关闭那一侧的历史凭空少掉一轮——差值随之被高估。
		taskID := "task-old-" + nth
		next(domain.SessionEventTurnStart, map[string]any{"turn": turn})
		next(domain.SessionEventUserMessage, map[string]any{
			"turn": turn, "turn_id": taskID + ":user", "task_id": taskID, "agent_id": "a1",
			"content": volumeUserMarker + " 第 " + nth + " 轮：读一下 notes.md 并说说缓存",
		})
		next(domain.SessionEventStepStart, map[string]any{"turn": turn, "step": 0})
		next(domain.SessionEventAssistantMessage, map[string]any{
			"turn": turn, "step": 0, "turn_id": taskID + ":assistant", "task_id": taskID, "agent_id": "a1",
			"content":       "我读一下 notes.md",
			"model_profile": "fast",
			"tool_calls": []any{
				map[string]any{"call_id": callID, "name": "read_file"},
			},
		})
		next(domain.SessionEventToolCall, map[string]any{
			"turn": turn, "step": 0, "call_id": callID, "name": "read_file",
			"arguments": "{\"path\":\"notes.md\"}",
		})
		next(domain.SessionEventToolResult, map[string]any{
			"turn": turn, "step": 0, "call_id": callID,
			"preview": volumeToolPreview(callID), "is_error": false,
			"spill_locator": volumeSpillPath,
		})
		next(domain.SessionEventStepEnd, map[string]any{"turn": turn, "step": 0, "reason": "completed"})
		next(domain.SessionEventStepStart, map[string]any{"turn": turn, "step": 1})
		next(domain.SessionEventAssistantMessage, map[string]any{
			"turn": turn, "step": 1, "turn_id": taskID + ":assistant", "task_id": taskID, "agent_id": "a1",
			"content": "读完了：热点键集中在前几个分片。", "model_profile": "fast",
		})
		next(domain.SessionEventStepEnd, map[string]any{"turn": turn, "step": 1, "reason": "completed"})
		next(domain.SessionEventTurnEnd, map[string]any{"turn": turn, "reason": "completed"})
	}
	if err := repo.Append(ctx, sessionID, events); err != nil {
		t.Fatalf("append volume history events: %v", err)
	}
}

// blockRecordingBuilder 把真 Core 建出来的 BuiltContext.Blocks 留下来。
//
// 度量必须接在生产那条路上：Blocks 是 Runtime 从 ContextBuilder 拿到、交给
// logContextBlocks 记录的那一份，直接在测试里另调一次 Core.BuildContext 量出来的
// 是另一个请求，证明不了「线上那次装配的归因是对的」。
type blockRecordingBuilder struct {
	inner  ContextBuilder
	mu     sync.Mutex
	builds [][]cognitive.BlockSize
}

func (b *blockRecordingBuilder) BuildContext(ctx context.Context, req cognitive.Request) (cognitive.BuiltContext, error) {
	built, err := b.inner.BuildContext(ctx, req)
	if err != nil {
		return cognitive.BuiltContext{}, err
	}
	b.mu.Lock()
	b.builds = append(b.builds, append([]cognitive.BlockSize(nil), built.Blocks...))
	b.mu.Unlock()
	return built, nil
}

// first 是这次任务**第一次**装配上下文时的分节核算。空了就是没装配过——那时
// 返回一份空切片会让下面每一条归因断言都变成空过，所以这里直接判失败。
func (b *blockRecordingBuilder) first(t *testing.T) []cognitive.BlockSize {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.builds) == 0 {
		t.Fatal("ContextBuilder 一次都没被调用：这次任务没有装配上下文")
	}
	return b.builds[0]
}

// volumeRun 是一次任务跑完后留下的两样可度量的东西：模型真正收到的 messages，
// 以及那次装配的分节核算。
type volumeRun struct {
	messages []port.InferenceMessage
	blocks   []cognitive.BlockSize
}

// runVolumeTask 用同一批历史事件、同一个夹具，只把 G3 开关拨一下，跑一个后续任务。
//
// 它走的是生产两条任务路径共用的那一处选路（SessionHistoryForTask）与真的
// cognitive.Core，所以量出来的是**线上这两种形状各自送出去多少字符**，不是测试
// 自己拼出来的两个字符串。
func runVolumeTask(t *testing.T, toolTranscript bool) volumeRun {
	t.Helper()
	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	seedVolumeHistorySession(t, ctx, repo, volumeSessionID)

	task := domain.Task{ID: "task-now", SessionID: volumeSessionID, AgentID: "a1", Input: "接着说"}
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
	builder := &blockRecordingBuilder{
		inner: cognitive.NewCore(cognitive.NoopCompressor{}).WithContextFiles(volumeContextFile),
	}
	rt := NewRuntime(Config{
		Gate:              taskgate.NewTaskGate(),
		Maas:              maas,
		Audit:             adapter.NewMemoryAuditLog(),
		ContextBuilder:    builder,
		MaxToolRounds:     3,
		ConversationTurns: history.Turns,
		HistoryTranscript: history.Transcript,
	})
	if _, err := rt.RunTask(ctx, domain.Agent{ID: "a1", Role: "developer"}, task); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	return volumeRun{messages: maas.first(t).Messages, blocks: builder.first(t)}
}

// P5 的判据（spec §9）：「打开后 token 体积变化可度量」。
//
// 这条测试就是那个度量：同一批历史事件，开与关两种形状各自送出去多少字符，
// 差值必须能算出来、且必须是正的（transcript 带上了工具往返，一定更大）。
//
// 它不是性能测试——不断言差值的具体数值（那会随夹具变），只断言**这个差值
// 是可测量的**，并把两个数字打进测试输出，让人能看见代价有多大。
func TestTheSwitchVolumeDifferenceIsMeasurable(t *testing.T) {
	t.Parallel()

	off := runVolumeTask(t, false)
	on := runVolumeTask(t, true)

	// totalChars 是本包给 conversation.render 用的同一个计量口径（内容的 rune 数），
	// 也是 debug 探针 logInferenceRequest 记的 total_content_chars 的口径。度量接在
	// 它上面，测试里量到的数与线上日志里读到的数才是同一个东西。
	offChars := totalChars(off.messages)
	onChars := totalChars(on.messages)
	if offChars <= 0 {
		t.Fatalf("关闭时送出去的字符数 = %d：夹具没把历史送进模型，下面的差值无意义", offChars)
	}
	if onChars <= 0 {
		t.Fatalf("打开时送出去的字符数 = %d：夹具没把历史送进模型，下面的差值无意义", onChars)
	}

	delta := onChars - offChars
	ratio := float64(onChars) / float64(offChars)
	t.Logf("off=%d on=%d delta=%d ratio=%.2fx", offChars, onChars, delta, ratio)

	if delta <= 0 {
		t.Errorf("delta = %d，要 > 0：打开 G3 后历史多带了工具往返，体积必须更大\noff=%d on=%d",
			delta, offChars, onChars)
	}

	// 两次跑的必须是**同一批历史**，否则「一边大一边小」可能只是因为另一边压根
	// 没读到历史——那种情况下差值仍然为正，却什么也没量到。
	offText := strings.Join(messageContents(off.messages), "\n")
	onText := strings.Join(messageContents(on.messages), "\n")
	if !strings.Contains(offText, volumeUserMarker) {
		t.Fatalf("关闭时模型没看到历史标记 %q：这一侧根本没有历史，差值不是形状之差", volumeUserMarker)
	}
	if !strings.Contains(onText, volumeUserMarker) {
		t.Fatalf("打开时模型没看到历史标记 %q：这一侧根本没有历史，差值不是形状之差", volumeUserMarker)
	}

	// 差值必须是**工具往返**撑起来的，否则「更大」可能来自任何别的东西，这条测试
	// 就不再是在量 G3 的代价。关闭时一条 tool 消息都不该有，打开时必须有——把这部分
	// 单独量出来，差值的来源才说得出名字。
	offToolChars := toolRoleChars(off.messages)
	onToolChars := toolRoleChars(on.messages)
	if offToolChars != 0 {
		t.Errorf("关闭时 tool 角色消息占了 %d 字符，要 0：关闭时历史不该以 transcript 进模型", offToolChars)
	}
	if onToolChars <= 0 {
		t.Fatalf("打开时 tool 角色消息占了 %d 字符，要 > 0：工具往返没进模型，差值不是 G3 的代价", onToolChars)
	}
	if !strings.Contains(onText, volumeToolMarker) {
		t.Errorf("打开时模型没看到历史工具输出标记 %q：量到的差值与 G3 无关", volumeToolMarker)
	}
	if strings.Contains(offText, volumeToolMarker) {
		t.Errorf("关闭时模型也看到了历史工具输出标记 %q：关闭那一侧的形状变了", volumeToolMarker)
	}
}

// 历史那一段的体积必须在 Blocks 里可归因——「prompt 涨了 2 KB」应该能回答
// 「是谁涨的」。这是本仓 plugin_prompt 段已经立下的规矩（core.go 的注释）。
//
// 关闭时历史在 prompt 里，归在 "conversation" 段下，Chars 就是它占的量；
// 打开时历史离开了 prompt，归在 "conversation_transcript" 段下、Chars 为 0，
// 它的体积转到 messages 上（上一条测试量的就是那部分）。两种情况下 Blocks 都
// 有且只有一项回答「历史这一段在 prompt 里占了多少、走的是哪条路」。
func TestHistoryVolumeIsAttributableInBlocks(t *testing.T) {
	t.Parallel()

	off := runVolumeTask(t, false)
	on := runVolumeTask(t, true)

	// --- 关闭：历史走 prompt ---
	offConversation, ok := findBlock(off.blocks, "conversation")
	if !ok {
		t.Fatalf("关闭时 Blocks 里没有 conversation 段：历史体积不可归因\n%+v", off.blocks)
	}
	if offConversation.Chars <= 0 {
		t.Errorf("关闭时 conversation 段 = %d 字符，要 > 0：历史确实在 prompt 里，归因却说没有", offConversation.Chars)
	}
	if _, present := findBlock(off.blocks, "conversation_transcript"); present {
		t.Errorf("关闭时出现了 conversation_transcript 段：历史并没有走 transcript\n%+v", off.blocks)
	}
	// 归因要是真的：那一段的大小必须与 prompt 里那段历史文本的实际长度对得上。
	promptHistory := historyTextInPrompt(t, off.messages[0].Content)
	if got := len([]rune(promptHistory)); got != offConversation.Chars {
		t.Errorf("conversation 段记的是 %d 字符，prompt 里那段历史实际 %d 字符：归因数字是假的",
			offConversation.Chars, got)
	}

	// --- 打开：历史走 transcript ---
	onTranscript, ok := findBlock(on.blocks, "conversation_transcript")
	if !ok {
		t.Fatalf("打开时 Blocks 里既没有 conversation 也没有 conversation_transcript 段：\n"+
			"「历史去哪了」不可归因——这正是本步要补的那一项\n%+v", on.blocks)
	}
	if onTranscript.Chars != 0 {
		t.Errorf("conversation_transcript 段 = %d 字符，要 0：打开时历史一个字都不在 prompt 里",
			onTranscript.Chars)
	}
	if _, present := findBlock(on.blocks, "conversation"); present {
		t.Errorf("打开时同时出现 conversation 段：历史被归了两次，也就是进了两次\n%+v", on.blocks)
	}
	if strings.Contains(on.messages[0].Content, "Recent conversation:") {
		t.Errorf("打开时 prompt 里还有 \"Recent conversation:\"：归因说历史走了 transcript，实际没走\n%s",
			on.messages[0].Content)
	}
	// 历史的体积转到了 messages 上，而不是消失了。
	if tail := totalChars(on.messages[1:]); tail <= 0 {
		t.Errorf("messages[1:] 一共 %d 字符：归因说历史走了 transcript，transcript 却是空的", tail)
	}

	// --- 两种情况下核算都必须是完整的 ---
	// Blocks 的契约（core.go）：各段之和等于装配出的 prompt 的 rune 数。补进来的那一
	// 项若把这条打破，Blocks 就从「体积核算」退化成「一份说明」。
	assertBlocksAccountForWholePrompt(t, "off", off)
	assertBlocksAccountForWholePrompt(t, "on", on)
}

// assertBlocksAccountForWholePrompt 核对分节之和等于 messages[0] 的实际长度。
//
// messages[0] 就是 buildPrompt 返回的 basePrompt 原样（本夹具没开 lazy 协议、
// 任务也不是 Plan 模式，两处会往后面追加文本的分支都不触发）。
func assertBlocksAccountForWholePrompt(t *testing.T, label string, run volumeRun) {
	t.Helper()
	sum := 0
	for _, b := range run.blocks {
		sum += b.Chars
	}
	if got := len([]rune(run.messages[0].Content)); sum != got {
		t.Errorf("[%s] Blocks 各段之和 = %d，messages[0] 实际 %d 字符：有一段 prompt 无人认领\n%+v",
			label, sum, got, run.blocks)
	}
}

// findBlock 按名字取一段核算。第二个返回值区分「有这一段但是 0」与「根本没有这一段」——
// 这两件事正是本步要分开的：前者是「历史没占体积」，后者是「不知道历史去哪了」。
func findBlock(blocks []cognitive.BlockSize, name string) (cognitive.BlockSize, bool) {
	for _, b := range blocks {
		if b.Name == name {
			return b, true
		}
	}
	return cognitive.BlockSize{}, false
}

// toolRoleChars 是 tool 角色消息的内容总量：G3 打开后多出来的那部分体积。
func toolRoleChars(msgs []port.InferenceMessage) int {
	total := 0
	for _, m := range msgs {
		if m.Role == port.RoleTool {
			total += len([]rune(m.Content))
		}
	}
	return total
}

// messageContents 取每条消息的正文，供包含性断言拼接。
func messageContents(msgs []port.InferenceMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

// historyTextInPrompt 截出 prompt 里 "Recent conversation:" 那一段（它是 Core
// 装配的最后一段，一直到 prompt 结束）。取不到就判失败：取不到却返回空串，会让
// 调用处那条「归因数字对不对」的比较变成 0 == 0 的空过。
func historyTextInPrompt(t *testing.T, prompt string) string {
	t.Helper()
	idx := strings.Index(prompt, "Recent conversation:")
	if idx < 0 {
		t.Fatalf("prompt 里没有 \"Recent conversation:\"：关闭时历史必须在 prompt 文本里\n%s", prompt)
	}
	return prompt[idx:]
}
