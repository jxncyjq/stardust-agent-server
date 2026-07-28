package runtime

import (
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

func msg(role, content string, calls []domain.ToolCall, toolCallID string) port.InferenceMessage {
	return port.InferenceMessage{Role: role, Content: content, ToolCalls: calls, ToolCallID: toolCallID}
}

func TestCompactionSplitAvoidsOrphanToolResult(t *testing.T) {
	// 0:base(user) 1:asst(callA) 2:tool(A) 3:asst(callB) 4:tool(B) 5:asst(text) 6:user
	msgs := []port.InferenceMessage{
		msg(port.RoleUser, "base", nil, ""),
		msg(port.RoleAssistant, "", []domain.ToolCall{{ID: "A"}}, ""),
		msg(port.RoleTool, "resA", nil, "A"),
		msg(port.RoleAssistant, "", []domain.ToolCall{{ID: "B"}}, ""),
		msg(port.RoleTool, "resB", nil, "B"),
		msg(port.RoleAssistant, "answer", nil, ""),
		msg(port.RoleUser, "next", nil, ""),
	}
	// preserveTail=2 → 目标 preserveStart=5(asst answer)，本就是安全边界(非 tool)
	cs, ps, ok := compactionSplit(msgs, 2)
	if !ok || cs != 1 || ps != 5 {
		t.Fatalf("got cs=%d ps=%d ok=%v, want 1,5,true", cs, ps, ok)
	}
	// preserveTail=3 → 目标 len-3=4 是 RoleTool(B) 孤儿 → 向前 walk 到 3(asst callB)
	cs, ps, ok = compactionSplit(msgs, 3)
	if !ok || ps != 3 {
		t.Fatalf("preserveTail=3: ps=%d ok=%v, want ps=3 (walked back off orphan tool)", ps, ok)
	}
	// 太短不压缩
	if _, _, ok := compactionSplit(msgs[:3], 2); ok {
		t.Fatalf("len<4 should not compact")
	}
}

func TestCompactionSplitNothingToCompact(t *testing.T) {
	msgs := []port.InferenceMessage{
		msg(port.RoleUser, "base", nil, ""),
		msg(port.RoleAssistant, "a", nil, ""),
		msg(port.RoleUser, "b", nil, ""),
		msg(port.RoleAssistant, "c", nil, ""),
	}
	// preserveTail 覆盖 base 之后全部 → 无可压缩
	if _, _, ok := compactionSplit(msgs, 3); ok {
		t.Fatalf("preserveTail covering all-after-base should be ok=false")
	}
}
