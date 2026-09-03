package runtime

import (
	"fmt"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
)

// sessionKeyForTask picks the directory key for a task's persisted state: its
// session id when it has one, otherwise its task id (one-shot tasks with no
// session still get an isolated checkpoint dir).
func sessionKeyForTask(task domain.Task) string {
	if task.SessionID != "" {
		return task.SessionID
	}
	return task.ID
}

// snapshotMessages converts the runtime's internal (unexported-field)
// conversation into the serialisable snapshot form for a checkpoint.
func snapshotMessages(convo *conversation) []sessionstate.MessageSnapshot {
	if convo == nil {
		return nil
	}
	out := make([]sessionstate.MessageSnapshot, 0, len(convo.messages))
	for _, msg := range convo.messages {
		out = append(out, sessionstate.MessageSnapshot{
			Role:       msg.Role,
			Content:    msg.Content,
			Images:     msg.Images,
			ToolCalls:  msg.ToolCalls,
			ToolCallID: msg.ToolCallID,
		})
	}
	return out
}

// restoreConversation rebuilds the exchange from a checkpoint snapshot, so a
// resumed loop continues from the same history the model was last shown.
// taskStart 必须一起带回来。它是重复熔断的扫描起点（见 conversation.taskStart）：
// 丢了它，续跑的任务会把 G3 注入的历史重新算进 streak，一条正常的会话就能把 streak
// 顶过 repeatWarnStreak，模型平白收到重复调用警告。这里是本包第二个构造 conversation
// 的地方，也是唯一一个不经 newConversation/appendHistory 的——新增字段时最容易漏掉
// 的正是它。
//
// taskStart 为 0 表示这份检查点写于该字段引入之前：那时 conversation 里不存在历史
// transcript 段，起点恒为 1，按 1 处理。这是契约声明的可选，不是替坏值兜底——一个
// 大过消息条数的下标是检查点损坏，直接 panic，因为将就下去只会让 panic 发生在离
// 现场很远的地方。
// 守卫：TestRestoringACheckpointKeepsTheTaskBoundary、
// TestACheckpointFromBeforeTheBoundaryFieldRestoresToOne、
// TestACorruptBoundaryFailsLoudAtRestore。
func restoreConversation(snaps []sessionstate.MessageSnapshot, taskStart int) *conversation {
	if taskStart == 0 {
		taskStart = 1
	}
	if taskStart < 0 || taskStart > len(snaps) {
		panic(fmt.Sprintf("runtime: checkpoint task_start=%d is out of range for %d messages; "+
			"the checkpoint is corrupt", taskStart, len(snaps)))
	}
	convo := &conversation{
		messages:  make([]port.InferenceMessage, 0, len(snaps)),
		taskStart: taskStart,
	}
	for _, s := range snaps {
		convo.messages = append(convo.messages, port.InferenceMessage{
			Role:       s.Role,
			Content:    s.Content,
			Images:     s.Images,
			ToolCalls:  s.ToolCalls,
			ToolCallID: s.ToolCallID,
		})
	}
	return convo
}

// snapshotLoaded converts the runtime's internal (unexported-field) loaded
// block into the serialisable checkpoint form, so a suspended run's pinned
// capability definitions survive a resume without having to be reloaded.
func snapshotLoaded(entries []loadedEntry) []sessionstate.LoadedCapability {
	out := make([]sessionstate.LoadedCapability, 0, len(entries))
	for _, e := range entries {
		out = append(out, sessionstate.LoadedCapability{Name: e.name, Detail: e.detail})
	}
	return out
}

// restoreLoaded rebuilds the internal loaded block from a checkpoint's Loaded
// snapshot. An empty/nil snaps is legitimate (fresh task, a run that never
// called load_capabilities, or a checkpoint written before this field
// existed) and restores to an empty loaded block, not an error.
func restoreLoaded(snaps []sessionstate.LoadedCapability) []loadedEntry {
	out := make([]loadedEntry, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, loadedEntry{name: s.Name, detail: s.Detail})
	}
	return out
}
