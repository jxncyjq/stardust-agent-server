package browser

import (
	"strings"
	"testing"
)

// rawA11yNode 是从 CDP Accessibility 域抽出的中间结构（Task 6 由 go-rod 填充）。
func node(role, name string, interactive, visible bool) RawA11yNode {
	return RawA11yNode{Role: role, Name: name, Interactive: interactive, Visible: visible}
}

func TestBuildObservationKeepsInteractiveVisibleAssignsStableRef(t *testing.T) {
	raw := []RawA11yNode{
		node("button", "搜索", true, true),
		node("StaticText", "无关文本", false, true), // 非交互 → 丢弃
		node("link", "隐藏链接", true, false),       // 不可见 → 丢弃
		node("textbox", "关键词框", true, true),
	}
	obs := BuildObservation(raw, ObservationBudget{MaxElements: 50})

	if len(obs.Elements) != 2 {
		t.Fatalf("kept %d elements, want 2 (interactive+visible only)", len(obs.Elements))
	}
	if obs.Elements[0].Ref != "e1" || obs.Elements[1].Ref != "e2" {
		t.Fatalf("refs = %q,%q, want e1,e2", obs.Elements[0].Ref, obs.Elements[1].Ref)
	}
	if !strings.Contains(obs.Text, "[e1]") || !strings.Contains(obs.Text, "搜索") {
		t.Fatalf("render missing ref/name: %q", obs.Text)
	}
	if obs.Truncated {
		t.Fatalf("should not be truncated under budget")
	}
}

func TestBuildObservationTruncatesAtMaxElements(t *testing.T) {
	var raw []RawA11yNode
	for i := 0; i < 10; i++ {
		raw = append(raw, node("button", "b", true, true))
	}
	obs := BuildObservation(raw, ObservationBudget{MaxElements: 3})
	if len(obs.Elements) != 3 {
		t.Fatalf("kept %d, want 3", len(obs.Elements))
	}
	if !obs.Truncated {
		t.Fatalf("expected Truncated=true when clipped")
	}
}
