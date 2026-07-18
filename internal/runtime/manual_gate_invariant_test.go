package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/tool"
)

// TestRunTaskManualWithoutGateFailsLoud asserts the Manual-mode invariant: a
// Manual task must never reach a runtime whose approval gate is unwired. A nil
// toolGate never suspends and a nil checkpoint store cannot persist a suspension,
// so either would let the task silently execute sensitive tools and bypass human
// approval. RunTask must fail loud with ErrManualGateMissing rather than degrade
// to Auto behaviour. Guards against a future path (e.g. a delegated child that
// inherits Mode=manual) constructing a gateless runtime.
func TestRunTaskManualWithoutGateFailsLoud(t *testing.T) {
	dir := t.TempDir()
	cpStore := sessionstate.NewStore(dir)

	tests := []struct {
		name        string
		toolGate    ToolGate
		checkpoints *sessionstate.Store
	}{
		{name: "nil tool gate", toolGate: nil, checkpoints: cpStore},
		{name: "nil checkpoints", toolGate: allowAllGate{}, checkpoints: nil},
		{name: "both nil", toolGate: nil, checkpoints: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRuntime(Config{
				Maas:        &captureMaas{response: "should never run"},
				Audit:       adapter.NewMemoryAuditLog(),
				Events:      adapter.NewMemoryEventBus(),
				ToolGate:    tt.toolGate,
				Checkpoints: tt.checkpoints,
			})
			task := domain.Task{ID: "t1", SessionID: "s1", AgentID: "a1", Mode: domain.ModeManual, Input: "go"}
			_, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task)
			if !errors.Is(err, ErrManualGateMissing) {
				t.Fatalf("RunTask err = %v, want ErrManualGateMissing", err)
			}
		})
	}
}

// TestRunTaskAutoWithoutGateOK asserts the invariant is scoped to Manual mode:
// an Auto task without a gate is legitimate (Auto never suspends) and must not
// trip ErrManualGateMissing.
func TestRunTaskAutoWithoutGateOK(t *testing.T) {
	r := NewRuntime(Config{
		Maas:   &captureMaas{response: "done"},
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
	})
	task := domain.Task{ID: "t1", SessionID: "s1", AgentID: "a1", Mode: domain.ModeAuto, Input: "go"}
	if _, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task); errors.Is(err, ErrManualGateMissing) {
		t.Fatalf("Auto task tripped ErrManualGateMissing: %v", err)
	}
}

// allowAllGate is a ToolGate that never suspends and allows every call. It lets
// the "nil checkpoints" case supply a non-nil gate so the test isolates the
// missing checkpoint store as the failing invariant.
type allowAllGate struct{}

func (allowAllGate) ShouldSuspend(context.Context, domain.Task, []domain.ToolCall, *tool.Registry) (bool, error) {
	return false, nil
}

func (allowAllGate) Resolve(context.Context, domain.Task, domain.ToolCall, *tool.Registry) (bool, error) {
	return true, nil
}
