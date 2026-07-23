package runtime

import (
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
func restoreConversation(snaps []sessionstate.MessageSnapshot) *conversation {
	convo := &conversation{messages: make([]port.InferenceMessage, 0, len(snaps))}
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
