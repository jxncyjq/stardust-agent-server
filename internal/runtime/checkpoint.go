package runtime

import (
	"github.com/stardust/legion-agent/internal/domain"
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

// snapshotToolEntries converts the runtime's internal (unexported-field) tool
// context into the serialisable snapshot form for a checkpoint.
func snapshotToolEntries(entries []toolEntry) []sessionstate.ToolEntrySnapshot {
	out := make([]sessionstate.ToolEntrySnapshot, 0, len(entries))
	for _, e := range entries {
		out = append(out, sessionstate.ToolEntrySnapshot{Key: e.key, Text: e.text})
	}
	return out
}

// restoreToolEntries rebuilds internal tool context from a checkpoint snapshot,
// so a resumed loop re-accumulates identical deduplicated context.
func restoreToolEntries(snaps []sessionstate.ToolEntrySnapshot) []toolEntry {
	out := make([]toolEntry, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, toolEntry{key: s.Key, text: s.Text})
	}
	return out
}
