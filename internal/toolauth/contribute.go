package toolauth

import (
	"fmt"
	"sync"
)

// contributed holds gateable tools registered at runtime by contributors the
// static table cannot know about — plugins, and anything else that adds tools
// after assembly. Keyed by tool name.
//
// The gateable set has to be the union of the builtin table and these: a tool
// missing from it is not merely absent from the config UI, it is a tool a
// per-agent disabled_tools list cannot reach at all. That is an authorization
// bypass, not a cosmetic gap.
var (
	contributedMu sync.RWMutex
	contributed   = make(map[string]GateableTool)
)

// Contribute registers one gateable tool at runtime and returns an idempotent
// revoke function that removes it again.
//
// A name that is already gateable — builtin or contributed — panics. Two
// contributors claiming one gateable name is never a valid state: the config UI
// would show a single entry that silently governs whichever tool happened to
// win, and the loser's entry could never be revoked.
func Contribute(tool GateableTool) func() {
	if tool.Name == "" {
		panic("toolauth: Contribute requires a tool name")
	}
	contributedMu.Lock()
	if _, exists := contributed[tool.Name]; exists || isBuiltinGateable(tool.Name) {
		contributedMu.Unlock()
		panic(fmt.Sprintf("toolauth: duplicate gateable contribution for %q", tool.Name))
	}
	contributed[tool.Name] = tool
	contributedMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			contributedMu.Lock()
			delete(contributed, tool.Name)
			contributedMu.Unlock()
		})
	}
}

// isBuiltinGateable reports whether name is in the static table.
func isBuiltinGateable(name string) bool {
	for _, t := range gateable {
		if t.Name == name {
			return true
		}
	}
	return false
}

// contributedTools returns a snapshot of the runtime contributions.
func contributedTools() []GateableTool {
	contributedMu.RLock()
	defer contributedMu.RUnlock()
	out := make([]GateableTool, 0, len(contributed))
	for _, t := range contributed {
		out = append(out, t)
	}
	return out
}
