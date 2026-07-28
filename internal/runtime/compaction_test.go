package runtime

import (
	"context"
	"fmt"
	"strings"
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

// fakeSummaryMaas is a port.MaasInferenceClient test double that either returns
// a canned summary or fails, and records the last request it saw.
type fakeSummaryMaas struct {
	out string
	err error
	got port.InferenceRequest
}

func (f *fakeSummaryMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	f.got = req
	if f.err != nil {
		return port.InferenceResponse{}, f.err
	}
	return port.InferenceResponse{Text: f.out}, nil
}

// TestCompactConversationReplacesAndPreservesBase uses compactPreserveTail=8, so
// the fixture needs more than 8+2 messages after the base prompt to actually
// trigger a compaction (see compactionSplit's preserveTail walk-back).
func TestCompactConversationReplacesAndPreservesBase(t *testing.T) {
	convo := &conversation{messages: []port.InferenceMessage{
		msg(port.RoleUser, "BASE-PROMPT", nil, ""),
		msg(port.RoleAssistant, "old1", nil, ""),
		msg(port.RoleUser, "old2", nil, ""),
		msg(port.RoleAssistant, "old3", nil, ""),
		msg(port.RoleUser, "old4", nil, ""),
		msg(port.RoleAssistant, "old5", nil, ""),
		msg(port.RoleUser, "old6", nil, ""),
		msg(port.RoleAssistant, "old7", nil, ""),
		msg(port.RoleUser, "old8", nil, ""),
		msg(port.RoleAssistant, "recentA", nil, ""),
		msg(port.RoleUser, "recentB", nil, ""),
		msg(port.RoleAssistant, "recentC", nil, ""),
	}}
	orig := len(convo.messages)
	r := &Runtime{maas: &fakeSummaryMaas{out: "SUMMARY-TEXT"}}
	ok, err := r.compactConversation(t.Context(), convo)
	if err != nil || !ok {
		t.Fatalf("compact = (%v,%v), want (true,nil)", ok, err)
	}
	if convo.messages[0].Content != "BASE-PROMPT" {
		t.Fatalf("base prompt must be untouched, got %q", convo.messages[0].Content)
	}
	if convo.messages[1].Role != port.RoleUser || !strings.Contains(convo.messages[1].Content, "SUMMARY-TEXT") {
		t.Fatalf("msg[1] should be summary, got %+v", convo.messages[1])
	}
	// 消息数应下降
	if len(convo.messages) >= orig {
		t.Fatalf("messages not reduced: %d >= %d", len(convo.messages), orig)
	}
}

// TestCompactConversationFailLoudKeepsHistory needs the same >8+2 message
// fixture so compactionSplit actually returns ok=true and the fake maas client
// is invoked — otherwise compactConversation would short-circuit to (false,
// nil) before ever calling Generate, and the fail-loud path under test would
// never run.
func TestCompactConversationFailLoudKeepsHistory(t *testing.T) {
	orig := []port.InferenceMessage{
		msg(port.RoleUser, "BASE", nil, ""),
		msg(port.RoleAssistant, "a", nil, ""),
		msg(port.RoleUser, "b", nil, ""),
		msg(port.RoleAssistant, "c", nil, ""),
		msg(port.RoleUser, "d", nil, ""),
		msg(port.RoleAssistant, "e", nil, ""),
		msg(port.RoleUser, "f", nil, ""),
		msg(port.RoleAssistant, "g", nil, ""),
		msg(port.RoleUser, "h", nil, ""),
		msg(port.RoleAssistant, "i", nil, ""),
		msg(port.RoleUser, "j", nil, ""),
		msg(port.RoleAssistant, "k", nil, ""),
	}
	convo := &conversation{messages: append([]port.InferenceMessage(nil), orig...)}
	r := &Runtime{maas: &fakeSummaryMaas{err: fmt.Errorf("boom")}}
	ok, err := r.compactConversation(t.Context(), convo)
	if ok || err == nil {
		t.Fatalf("LLM 失败应 (false,err), got (%v,%v)", ok, err)
	}
	if len(convo.messages) != len(orig) {
		t.Fatalf("失败后历史被改动: %d != %d", len(convo.messages), len(orig))
	}
}

// TestCompactConversationEmptySummaryFailLoud covers I-1: a successful Generate
// that returns an empty summary must be treated as a failure (not applied), or
// real history would be replaced by an empty "[对话摘要]" — a cleared-history
// fallback smuggled in via a zero value.
func TestCompactConversationEmptySummaryFailLoud(t *testing.T) {
	orig := []port.InferenceMessage{
		msg(port.RoleUser, "BASE", nil, ""),
		msg(port.RoleAssistant, "a", nil, ""),
		msg(port.RoleUser, "b", nil, ""),
		msg(port.RoleAssistant, "c", nil, ""),
		msg(port.RoleUser, "d", nil, ""),
		msg(port.RoleAssistant, "e", nil, ""),
		msg(port.RoleUser, "f", nil, ""),
		msg(port.RoleAssistant, "g", nil, ""),
		msg(port.RoleUser, "h", nil, ""),
		msg(port.RoleAssistant, "i", nil, ""),
		msg(port.RoleUser, "j", nil, ""),
		msg(port.RoleAssistant, "k", nil, ""),
	}
	convo := &conversation{messages: append([]port.InferenceMessage(nil), orig...)}
	r := &Runtime{maas: &fakeSummaryMaas{out: "   "}} // whitespace-only = empty
	ok, err := r.compactConversation(t.Context(), convo)
	if ok || err == nil {
		t.Fatalf("空摘要应 (false,err), got (%v,%v)", ok, err)
	}
	if len(convo.messages) != len(orig) {
		t.Fatalf("空摘要后历史被改动: %d != %d", len(convo.messages), len(orig))
	}
}
