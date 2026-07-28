package runtime

import "github.com/stardust/legion-agent/internal/port"

// compactionSplit computes the range of messages that may be summarised away.
// msgs[0] (the base prompt / stable cache prefix) is always pinned, so
// compactStart is always 1. preserveStart is the index at which the preserved
// recent tail begins: it starts at len-preserveTail and is walked backward until
// it lands on a turn boundary that is NOT a RoleTool message — a RoleTool at the
// tail boundary would be an orphan whose RoleAssistant tool_calls fell into the
// compacted range, which providers reject. ok is false when there is nothing
// worth compacting (fewer than 4 messages, or the preserved tail already covers
// everything after the base prompt).
func compactionSplit(msgs []port.InferenceMessage, preserveTail int) (compactStart, preserveStart int, ok bool) {
	compactStart = 1
	if len(msgs) < 4 || preserveTail < 1 {
		return 0, 0, false
	}
	preserveStart = len(msgs) - preserveTail
	if preserveStart < compactStart {
		preserveStart = compactStart
	}
	// Walk backward off any orphan RoleTool boundary (its assistant is earlier).
	for preserveStart > compactStart && msgs[preserveStart].Role == port.RoleTool {
		preserveStart--
	}
	if preserveStart <= compactStart {
		return 0, 0, false
	}
	return compactStart, preserveStart, true
}
