package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/domain"
)

// blockingStreamRunner returns a StreamingRunner that parks until ctx is
// cancelled and then reports ctx.Err(), standing in for a real task run that
// is waiting on an approval decision.
func blockingStreamRunner(started chan<- struct{}) StreamingRunner {
	return func(ctx context.Context, _ string, _ func(domain.RuntimeEvent)) (app.DemoResult, error) {
		close(started)
		<-ctx.Done()
		return app.DemoResult{}, ctx.Err()
	}
}

// TestInteractiveApprovalUnblocksOnTaskContextCancel is the regression test
// for the leak that becomes reachable once run() gets a cancellable context:
// a Manual-mode approval is displayed, the parent context is cancelled, the
// gate leaves its decisionCh receive via ctx.Done() — and then the human
// presses "y". Without the select in sendApprovalDecision that keypress would
// park the cmd goroutine forever on an unbuffered channel nobody reads.
//
// Every assertion below is bounded by a timeout, so a goroutine that fails to
// unwind (the gate, the runner, the cancellation watcher, or the decision
// send) fails the test rather than leaking silently.
func TestInteractiveApprovalUnblocksOnTaskContextCancel(t *testing.T) {
	t.Parallel()

	reg := sensitiveToolRegistry(t, "write_file")
	pendingCh := make(chan PendingApproval)
	decisionCh := make(chan ApprovalDecision)
	defer close(pendingCh) // lets any re-issued waitApproval cmd return

	gate := NewApprovalGate(pendingCh, decisionCh)
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	started := make(chan struct{})
	model := NewInteractiveModel(InteractiveConfig{
		Context:         base,
		ApprovalCh:      pendingCh,
		DecisionCh:      decisionCh,
		StreamingRunner: blockingStreamRunner(started),
	})

	m, cmd := model.beginTask("write the notes")
	if cmd == nil {
		t.Fatalf("beginTask cmd = nil, want a batch")
	}
	if m.taskCtx == nil {
		t.Fatalf("beginTask taskCtx = nil, want a cancellable per-task context")
	}

	// The runner must receive the per-task context, not context.Background().
	runDone := make(chan tea.Msg, 1)
	go func() { runDone <- m.run("write the notes")() }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("streaming runner did not start within 2s")
	}

	// The gate blocks on the same context, publishes the pending approval,
	// and the model renders it.
	type outcome struct {
		allow bool
		err   error
	}
	gateDone := make(chan outcome, 1)
	go func() {
		allow, err := gate.Resolve(m.taskCtx, domain.Task{ID: "t1", Mode: domain.ModeManual},
			domain.ToolCall{ID: "c1", Name: "write_file", Arguments: map[string]string{"path": "notes.md"}}, reg)
		gateDone <- outcome{allow: allow, err: err}
	}()

	pendingMsg := make(chan tea.Msg, 1)
	go func() { pendingMsg <- m.waitApproval()() }()
	var pending tea.Msg
	select {
	case pending = <-pendingMsg:
	case <-time.After(2 * time.Second):
		t.Fatal("waitApproval did not deliver the pending approval within 2s")
	}
	next, _ := m.Update(pending)
	m = next.(InteractiveModel)
	if !m.approvalActive {
		t.Fatalf("approvalActive = false after pending approval, want true")
	}

	// Cancel the parent while the approval is still on screen.
	watcher := make(chan tea.Msg, 1)
	go func() { watcher <- m.watchTaskCancel()() }()
	cancelBase()

	select {
	case res := <-gateDone:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("gate.Resolve err = %v, want context.Canceled", res.err)
		}
		if res.allow {
			t.Fatalf("gate.Resolve allow = true on cancellation, want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gate.Resolve did not return within 2s of cancellation (deadlock)")
	}

	select {
	case msg := <-runDone:
		done, ok := msg.(interactiveRunDoneMsg)
		if !ok {
			t.Fatalf("run cmd returned %T, want interactiveRunDoneMsg", msg)
		}
		if !errors.Is(done.err, context.Canceled) {
			t.Fatalf("run err = %v, want context.Canceled (run must use the task context)", done.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run cmd did not return within 2s of cancellation (runner still on context.Background)")
	}

	// The human presses "y" after the cancellation: the decision has nowhere
	// to go, and the cmd must report that instead of parking forever.
	next, keyCmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	afterKey := next.(InteractiveModel)
	if afterKey.approvalActive {
		t.Fatalf("approvalActive = true after y, want false")
	}
	if keyCmd == nil {
		t.Fatalf("Update(y) cmd = nil, want a batch")
	}
	sendDone := make(chan tea.Msg, 1)
	go func() { sendDone <- afterKey.sendApprovalDecision(true)() }()
	select {
	case msg := <-sendDone:
		aborted, ok := msg.(interactiveApprovalAbortedMsg)
		if !ok {
			t.Fatalf("sendApprovalDecision returned %T, want interactiveApprovalAbortedMsg", msg)
		}
		if !errors.Is(aborted.err, context.Canceled) {
			t.Fatalf("aborted.err = %v, want context.Canceled", aborted.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendApprovalDecision did not return within 2s after cancellation (leaked cmd goroutine)")
	}

	// The cancellation watcher clears the stale prompt and says why.
	var cancelMsg tea.Msg
	select {
	case cancelMsg = <-watcher:
	case <-time.After(2 * time.Second):
		t.Fatal("watchTaskCancel did not fire within 2s of cancellation")
	}
	next, _ = m.Update(cancelMsg)
	m = next.(InteractiveModel)
	if m.approvalActive {
		t.Fatalf("approvalActive = true after task cancellation, want false (stale prompt)")
	}
	if !strings.Contains(m.err, "审批已中止") {
		t.Fatalf("err = %q, want the cancellation to be reported", m.err)
	}
}

// TestInteractiveQuitCancelsPendingApproval asserts the quit path unwinds a
// blocked gate: pressing Esc while an approval is displayed cancels the task
// context, so Resolve returns ctx.Err() instead of waiting on an answer the
// exiting program will never send.
func TestInteractiveQuitCancelsPendingApproval(t *testing.T) {
	t.Parallel()

	reg := sensitiveToolRegistry(t, "write_file")
	pendingCh := make(chan PendingApproval)
	decisionCh := make(chan ApprovalDecision)
	gate := NewApprovalGate(pendingCh, decisionCh)

	model := NewInteractiveModel(InteractiveConfig{
		Context:         context.Background(),
		ApprovalCh:      pendingCh,
		DecisionCh:      decisionCh,
		StreamingRunner: blockingStreamRunner(make(chan struct{})),
	})
	m, _ := model.beginTask("write the notes")

	errCh := make(chan error, 1)
	go func() {
		_, err := gate.Resolve(m.taskCtx, domain.Task{ID: "t1", Mode: domain.ModeManual},
			domain.ToolCall{ID: "c1", Name: "write_file"}, reg)
		errCh <- err
	}()
	pendingMsg := make(chan tea.Msg, 1)
	go func() { pendingMsg <- m.waitApproval()() }()
	select {
	case msg := <-pendingMsg:
		next, _ := m.Update(msg)
		m = next.(InteractiveModel)
	case <-time.After(2 * time.Second):
		t.Fatal("waitApproval did not deliver the pending approval within 2s")
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(InteractiveModel)
	if !m.quitting {
		t.Fatalf("quitting = false after Esc, want true")
	}
	if m.approvalActive {
		t.Fatalf("approvalActive = true after quit, want false")
	}
	if m.taskCtx != nil {
		t.Fatalf("taskCtx = non-nil after quit, want released")
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("gate.Resolve err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gate.Resolve did not return within 2s of quitting (deadlock)")
	}
}

// TestInteractiveRunDoneClearsApprovalAndTaskContext asserts that a finished
// task releases its context and takes any leftover approval prompt with it —
// the gate that raised it is gone, so the question is no longer answerable.
func TestInteractiveRunDoneClearsApprovalAndTaskContext(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{Context: context.Background()})
	m, _ := model.beginTask("do it")
	taskCtx := m.taskCtx

	next, _ := m.Update(interactivePendingApprovalMsg{Tool: "write_file"})
	m = next.(InteractiveModel)
	if !m.approvalActive {
		t.Fatalf("approvalActive = false, want true before the run finishes")
	}

	next, _ = m.Update(interactiveRunDoneMsg{})
	m = next.(InteractiveModel)
	if m.approvalActive {
		t.Fatalf("approvalActive = true after run done, want false")
	}
	if m.taskCtx != nil || m.taskCancel != nil {
		t.Fatalf("task context not released after run done")
	}
	select {
	case <-taskCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("finished task's context was not cancelled (leaked watcher goroutine)")
	}
}

// TestInteractiveTaskCancelledMsgIgnoresStaleSeq asserts a cancellation
// watcher belonging to an already-replaced task cannot clear the current
// task's approval prompt.
func TestInteractiveTaskCancelledMsgIgnoresStaleSeq(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{Context: context.Background()})
	m, _ := model.beginTask("first")
	staleSeq := m.taskSeq
	m, _ = m.beginTask("second")
	next, _ := m.Update(interactivePendingApprovalMsg{Tool: "write_file"})
	m = next.(InteractiveModel)

	next, _ = m.Update(interactiveTaskCancelledMsg{seq: staleSeq, err: context.Canceled})
	m = next.(InteractiveModel)
	if !m.approvalActive {
		t.Fatalf("approvalActive = false, want true (stale watcher must be ignored)")
	}
	if m.err != "" {
		t.Fatalf("err = %q, want empty (stale watcher must not report)", m.err)
	}
}
