package tool

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
)

func executedOrigin(t *testing.T, ctx context.Context) string {
	t.Helper()
	audit := adapter.NewMemoryAuditLog()
	r := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{}).WithAuditLog(audit)
	r.Register("probe", okHandler())

	if _, err := r.Execute(ctx, domain.Agent{ID: "a1"}, domain.ToolCall{ID: "c1", Name: "probe"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	events, err := audit.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, ev := range events {
		if ev.Action == "tool_executed" {
			return ev.Origin
		}
	}
	t.Fatal("no tool_executed audit event")
	return ""
}

// An unmarked call is the agent's own: the default must be a real value, not
// an empty string that reads as "unknown" in the audit trail.
func TestAuditOriginDefaultsToAgent(t *testing.T) {
	if got := executedOrigin(t, context.Background()); got != OriginAgent {
		t.Fatalf("want origin %q for an unmarked call, got %q", OriginAgent, got)
	}
}

func TestAuditOriginCarriesTheMarkedOrigin(t *testing.T) {
	ctx := WithCallOrigin(context.Background(), "plugin:jira")
	if got := executedOrigin(t, ctx); got != "plugin:jira" {
		t.Fatalf("want origin %q, got %q", "plugin:jira", got)
	}
}

// A failing tool must be attributable too — otherwise the noisiest rows in the
// audit trail are exactly the ones with no owner.
func TestAuditOriginIsRecordedOnFailure(t *testing.T) {
	audit := adapter.NewMemoryAuditLog()
	r := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{}).WithAuditLog(audit)
	r.Register("boom", HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, context.DeadlineExceeded
	}))

	ctx := WithCallOrigin(context.Background(), "plugin:jira")
	if _, err := r.Execute(ctx, domain.Agent{ID: "a1"}, domain.ToolCall{ID: "c1", Name: "boom"}); err == nil {
		t.Fatal("want the handler error to surface")
	}
	events, err := audit.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, ev := range events {
		if ev.Action == "tool_failed" {
			if ev.Origin != "plugin:jira" {
				t.Fatalf("want failure origin %q, got %q", "plugin:jira", ev.Origin)
			}
			return
		}
	}
	t.Fatal("no tool_failed audit event")
}

func TestCallOriginFromRejectsEmptyMarking(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic when marking a call with an empty origin")
		}
	}()
	WithCallOrigin(context.Background(), "")
}
