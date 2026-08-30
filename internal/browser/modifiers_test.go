package browser

import (
	"strings"
	"testing"
)

// 接管的前提是「人能像用自己的浏览器一样用它」，而快捷键是其中一部分。
//
// 在这些测试之前，修饰键**根本无法表达**：InputEvent 没有字段，键名白名单里也没有
// Control/Shift/Alt/Meta，于是 GUI 每按一次 Shift 都发出一条必定失败的请求，而
// Ctrl+C 会退化成「往页面里输入一个字母 c」——`keydown Control` 被拒，随后的 c 作为
// char 插进了页面。

func TestAModifierTravelsWithTheEventItModifies(t *testing.T) {
	t.Parallel()

	err := validateInputEvents([]InputEvent{
		{Type: "keydown", Key: "c", Modifiers: []string{"ctrl"}},
		{Type: "keyup", Key: "c", Modifiers: []string{"ctrl"}},
	})
	if err != nil {
		t.Errorf("validateInputEvents(ctrl+c) = %v, want nil: this is the shape a copy is", err)
	}
}

func TestEveryEventTypeAcceptsModifiers(t *testing.T) {
	t.Parallel()

	events := []InputEvent{
		{Type: "click", X: 0.5, Y: 0.5, Modifiers: []string{"ctrl"}},             // ctrl+click: open in a new tab
		{Type: "mousedown", X: 0.1, Y: 0.1, Modifiers: []string{"shift"}},        // shift+drag: extend a selection
		{Type: "wheel", X: 0.5, Y: 0.5, DeltaY: -3, Modifiers: []string{"ctrl"}}, // ctrl+wheel: zoom
		{Type: "keydown", Key: "ArrowLeft", Modifiers: []string{"shift", "alt"}},
	}
	if err := validateInputEvents(events); err != nil {
		t.Errorf("validateInputEvents = %v, want nil", err)
	}
}

// TestAnUnknownModifierIsRefusedByName: guessing what "cmd" or "ctrlKey" meant
// would inject a shortcut nobody asked for.
func TestAnUnknownModifierIsRefusedByName(t *testing.T) {
	t.Parallel()

	err := validateInputEvents([]InputEvent{{Type: "keydown", Key: "c", Modifiers: []string{"cmd"}}})
	if err == nil || !strings.Contains(err.Error(), "cmd") {
		t.Errorf("validateInputEvents(cmd) = %v, want a refusal naming the modifier", err)
	}
}

// TestAModifierIsNotAKey: a client that sends `keydown Control` is using the
// old shape. The refusal has to say where the modifier goes now, or the author
// reads "unsupported key" and concludes shortcuts are simply unsupported —
// which is exactly what the old message did.
func TestAModifierIsNotAKey(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"Control", "Shift", "Alt", "Meta"} {
		err := validateInputEvents([]InputEvent{{Type: "keydown", Key: key}})
		if err == nil {
			t.Errorf("validateInputEvents(keydown %s) = nil, want a refusal", key)
			continue
		}
		if !strings.Contains(err.Error(), "modifiers") {
			t.Errorf("validateInputEvents(keydown %s) = %q, want it to point at the modifiers field", key, err)
		}
	}
}

// TestACharEventRefusesModifiers is the defect stated as a rule. A char event
// is InsertText — it puts the literal text in the page and no modifier can
// change that — so accepting "ctrl" on it would silently type a "c" where the
// user meant to copy.
func TestACharEventRefusesModifiers(t *testing.T) {
	t.Parallel()

	err := validateInputEvents([]InputEvent{{Type: "char", Text: "c", Modifiers: []string{"ctrl"}}})
	if err == nil || !strings.Contains(err.Error(), "char") {
		t.Errorf("validateInputEvents(char+ctrl) = %v, want a refusal telling the caller to send a key event", err)
	}
	// Shift is the exception that proves it: the shifted character is ALREADY
	// in the text, so it carries no separate meaning and is not an error.
	if err := validateInputEvents([]InputEvent{{Type: "char", Text: "A", Modifiers: []string{"shift"}}}); err != nil {
		t.Errorf("validateInputEvents(char+shift) = %v, want nil: the text is already shifted", err)
	}
}

func TestModifierKeysMapToRealKeys(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"ctrl", "shift", "alt", "meta"} {
		if _, err := modifierToInputKey(name); err != nil {
			t.Errorf("modifierToInputKey(%q) = %v, want a key", name, err)
		}
	}
	if _, err := modifierToInputKey("ctrlKey"); err == nil {
		t.Error("modifierToInputKey(ctrlKey) = nil error, want a refusal")
	}
}
