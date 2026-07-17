package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/task"
)

type stubTrustGate struct {
	decision quality.TrustDecision
}

func (g stubTrustGate) CanExecute(context.Context, string, quality.RiskLevel, time.Time) (quality.TrustDecision, error) {
	return g.decision, nil
}

func newTrustCoordinator(t *testing.T, gate TrustGate, runner TaskRunner) (*Coordinator, *task.Scheduler, *adapter.MemoryAuditLog) {
	t.Helper()
	scheduler := task.NewScheduler()
	if err := scheduler.Add(context.Background(), domain.Task{
		ID:        "task-trust",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Status:    domain.TaskPending,
		Input:     "do work",
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	coordinator := NewCoordinator(CoordinatorConfig{
		Agent:     domain.Agent{ID: "agent-1", CompanyID: "company-1", Role: "developer", Status: domain.AgentActive},
		Scheduler: scheduler,
		Locks:     task.NewLockStore(),
		Runtime:   runner,
		Reviewer:  quality.NewAegisReviewer(),
		Evaluator: quality.NewEvalEngine(3),
		Approvals: approval.NewService(),
		Audit:     audit,
		Events:    events,
		TrustGate: gate,
		LockTTL:   time.Minute,
	})
	return coordinator, scheduler, audit
}

// TestCoordinatorSuspendsTrustBlockedTask asserts the L6 trust gate: a blocked
// agent must have its task suspended (for review) and never executed, with the
// reason recorded — not silently dropped or run anyway.
func TestCoordinatorSuspendsTrustBlockedTask(t *testing.T) {
	t.Parallel()

	runner := &recordingTaskRunner{result: "should not run"}
	coordinator, scheduler, audit := newTrustCoordinator(t, stubTrustGate{decision: quality.TrustDecisionBlocked}, runner)

	_, ok, err := coordinator.Heartbeat(context.Background())
	if err != nil {
		t.Fatalf("Heartbeat() error = %v, want nil", err)
	}
	if !ok {
		t.Fatal("Heartbeat() ok = false, want true")
	}
	// Block until the dispatched goroutine fully finishes (status transition,
	// audit, learning events) before asserting on any of its side effects —
	// awaiting only the terminal status is not enough, since audit/event
	// writes happen after the transition inside the same goroutine.
	coordinator.Wait()
	stored := awaitTerminal(t, scheduler, "task-trust")
	if stored.Status != domain.TaskSuspended {
		t.Errorf("stored status = %q, want %q", stored.Status, domain.TaskSuspended)
	}
	if runner.calls != 0 {
		t.Errorf("runner called %d times, want 0 (trust-blocked task must not run)", runner.calls)
	}
	if !hasAuditAction(audit.Events(), "trust_blocked") {
		t.Errorf("audit actions missing %q: %#v", "trust_blocked", audit.Events())
	}
}

// TestCoordinatorRunsTaskWhenTrustAllows asserts a trusted agent (the default
// score is in the allow band) runs its task normally through the gate.
func TestCoordinatorRunsTaskWhenTrustAllows(t *testing.T) {
	t.Parallel()

	runner := &recordingTaskRunner{result: "ok"}
	// Real manager, no negative events: default score sits in the allow band.
	coordinator, scheduler, _ := newTrustCoordinator(t, quality.NewTrustScoreManager(), runner)

	_, ok, err := coordinator.Heartbeat(context.Background())
	if err != nil {
		t.Fatalf("Heartbeat() error = %v, want nil", err)
	}
	if !ok {
		t.Fatal("Heartbeat() ok = false, want true")
	}
	// Block until the dispatched goroutine fully finishes (status transition,
	// audit, learning events) before asserting on any of its side effects —
	// awaiting only the terminal status is not enough, since audit/event
	// writes happen after the transition inside the same goroutine.
	coordinator.Wait()
	result := awaitTerminal(t, scheduler, "task-trust")
	if runner.calls != 1 {
		t.Errorf("runner called %d times, want 1 (trusted task should run)", runner.calls)
	}
	if result.Status != domain.TaskDone {
		t.Errorf("Heartbeat() status = %q, want %q", result.Status, domain.TaskDone)
	}
}
