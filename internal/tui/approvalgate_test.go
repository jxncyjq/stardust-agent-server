package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/tool"
)

// sensitiveToolRegistry returns a registry with a single sensitive tool
// registered under toolName, so approvalGate's sensitivity lookup has
// something real to find.
func sensitiveToolRegistry(t *testing.T, toolName string) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{})
	reg.RegisterDescriptor(tool.Descriptor{Name: toolName, Sensitive: true}, tool.HandlerFunc(
		func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{Success: true}, nil
		}))
	return reg
}

// TestApprovalGateShouldSuspendAlwaysFalse asserts 方案 Y's defining trait:
// the gate never drives round-level suspend/checkpoint — that's
// manualgate.ManualToolGate's job. Any task/calls combination must report
// (false, nil).
func TestApprovalGateShouldSuspendAlwaysFalse(t *testing.T) {
	gate := NewApprovalGate(make(chan PendingApproval), make(chan ApprovalDecision))
	task := domain.Task{ID: "t1", Mode: domain.ModeManual}
	calls := []domain.ToolCall{{ID: "c1", Name: "write_file"}}
	suspend, err := gate.ShouldSuspend(context.Background(), task, calls, nil)
	if err != nil {
		t.Fatalf("ShouldSuspend err = %v, want nil", err)
	}
	if suspend {
		t.Fatalf("ShouldSuspend = true, want false (方案 Y never round-suspends)")
	}
}

// TestApprovalGateResolveAllowsNonManual asserts that a non-Manual task's
// sensitive tool call is allowed immediately, never touching the channels.
// pendingCh/decisionCh below are unbuffered and never serviced by any other
// goroutine, so if Resolve tried to use them for a non-Manual call this test
// would hang until `go test`'s timeout kills it — that failure mode is the
// proof the "never touch" contract holds.
func TestApprovalGateResolveAllowsNonManual(t *testing.T) {
	reg := sensitiveToolRegistry(t, "write_file")
	gate := NewApprovalGate(make(chan PendingApproval), make(chan ApprovalDecision))
	task := domain.Task{ID: "t1", Mode: domain.ModeAuto}
	call := domain.ToolCall{ID: "c1", Name: "write_file"}
	allow, err := gate.Resolve(context.Background(), task, call, reg)
	if err != nil {
		t.Fatalf("Resolve err = %v, want nil", err)
	}
	if !allow {
		t.Fatalf("Resolve = false, want true (non-Manual never blocks)")
	}
}

// TestApprovalGateResolveBlocksAndReturnsDecision drives Resolve from a
// goroutine (it blocks on the real channel pair), receives the published
// PendingApproval on the main test goroutine, answers on decisionCh, and
// asserts Resolve unblocks with the matching decision — for both an approve
// and a deny answer.
func TestApprovalGateResolveBlocksAndReturnsDecision(t *testing.T) {
	reg := sensitiveToolRegistry(t, "write_file")
	pendingCh := make(chan PendingApproval)
	decisionCh := make(chan ApprovalDecision)
	gate := NewApprovalGate(pendingCh, decisionCh)
	task := domain.Task{ID: "t1", Mode: domain.ModeManual}
	call := domain.ToolCall{ID: "c1", Name: "write_file", Arguments: map[string]string{"path": "x"}}

	for _, tc := range []struct {
		name  string
		allow bool
	}{
		{name: "approve", allow: true},
		{name: "deny", allow: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type outcome struct {
				allow bool
				err   error
			}
			resultCh := make(chan outcome, 1)
			go func() {
				allow, err := gate.Resolve(context.Background(), task, call, reg)
				resultCh <- outcome{allow: allow, err: err}
			}()

			select {
			case pending := <-pendingCh:
				if pending.Tool != "write_file" {
					t.Errorf("pending.Tool = %q, want write_file", pending.Tool)
				}
				if pending.Args["path"] != "x" {
					t.Errorf("pending.Args[\"path\"] = %q, want x", pending.Args["path"])
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Resolve did not publish a pending approval within 2s")
			}

			decisionCh <- ApprovalDecision{Allow: tc.allow}

			select {
			case res := <-resultCh:
				if res.err != nil {
					t.Fatalf("Resolve err = %v, want nil", res.err)
				}
				if res.allow != tc.allow {
					t.Fatalf("Resolve allow = %v, want %v", res.allow, tc.allow)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Resolve did not return within 2s of the decision")
			}
		})
	}
}

// TestApprovalGateResolveCtxCancelFailsLoud asserts that cancelling ctx while
// Resolve is blocked (nobody ever answers pendingCh/decisionCh) unblocks it
// with (false, ctx.Err()) instead of deadlocking or silently allowing the
// call.
func TestApprovalGateResolveCtxCancelFailsLoud(t *testing.T) {
	reg := sensitiveToolRegistry(t, "write_file")
	pendingCh := make(chan PendingApproval)   // never serviced
	decisionCh := make(chan ApprovalDecision) // never serviced
	gate := NewApprovalGate(pendingCh, decisionCh)
	task := domain.Task{ID: "t1", Mode: domain.ModeManual}
	call := domain.ToolCall{ID: "c1", Name: "write_file"}

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		_, err := gate.Resolve(ctx, task, call, reg)
		doneCh <- err
	}()
	cancel()

	select {
	case err := <-doneCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Resolve err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve did not return after ctx cancellation within 2s (deadlock)")
	}
}
