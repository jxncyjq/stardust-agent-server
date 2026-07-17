package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/task"
)

func TestCoordinatorRunsPendingTaskToDone(t *testing.T) {
	t.Parallel()

	scheduler := task.NewScheduler()
	if err := scheduler.Add(context.Background(), domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		Status:    domain.TaskPending,
		Input:     "ship it",
	}); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	coordinator := NewCoordinator(CoordinatorConfig{
		Agent: domain.Agent{
			ID:        "agent-1",
			CompanyID: "company-1",
			Role:      "developer",
			Status:    domain.AgentActive,
		},
		Scheduler: scheduler,
		Locks:     task.NewLockStore(),
		Runtime: NewRuntime(Config{
			Maas:   adapter.NewRecordingMaas("safe result"),
			Audit:  audit,
			Events: events,
		}),
		Reviewer:  quality.NewAegisReviewer(),
		Evaluator: quality.NewEvalEngine(3),
		Approvals: approval.NewService(),
		Audit:     audit,
		Events:    events,
		LockTTL:   time.Minute,
	})

	result, ok, err := coordinator.Heartbeat(context.Background())
	if err != nil {
		t.Fatalf("Heartbeat() error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("Heartbeat() ok = false, want true")
	}
	if result.Status != domain.TaskDone {
		t.Errorf("Heartbeat() status = %q, want %q", result.Status, domain.TaskDone)
	}
	stored, _, err := scheduler.Get(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Get(%q) error = %v, want nil", "task-1", err)
	}
	if stored.Status != domain.TaskDone {
		t.Errorf("stored task status = %q, want %q", stored.Status, domain.TaskDone)
	}
	if !hasAuditAction(audit.Events(), "quality_approved") {
		t.Errorf("audit actions missing %q: %#v", "quality_approved", audit.Events())
	}
}

func TestCoordinatorSuspendsHardLoopAndCreatesApproval(t *testing.T) {
	t.Parallel()

	scheduler := task.NewScheduler()
	if err := scheduler.Add(context.Background(), domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		Status:    domain.TaskPending,
		Input:     "loop",
	}); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	approvals := approval.NewService()
	coordinator := NewCoordinator(CoordinatorConfig{
		Agent: domain.Agent{
			ID:        "agent-1",
			CompanyID: "company-1",
			Role:      "developer",
			Status:    domain.AgentActive,
		},
		Scheduler: scheduler,
		Locks:     task.NewLockStore(),
		Runtime: NewRuntime(Config{
			Maas:   adapter.NewRecordingMaas("same"),
			Audit:  audit,
			Events: events,
		}),
		Reviewer:  quality.NewAegisReviewer(),
		Evaluator: quality.NewEvalEngine(2),
		Approvals: approvals,
		Audit:     audit,
		Events:    events,
		LockTTL:   time.Minute,
	})

	result, ok, err := coordinator.Heartbeat(context.Background())
	if err != nil {
		t.Fatalf("Heartbeat() error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("Heartbeat() ok = false, want true")
	}
	if result.Status != domain.TaskSuspended {
		t.Errorf("Heartbeat() status = %q, want %q", result.Status, domain.TaskSuspended)
	}
	tickets := approvals.Tickets()
	if len(tickets) != 1 {
		t.Fatalf("approval tickets = %d, want 1", len(tickets))
	}
	if tickets[0].Type != approval.TicketHardLoop {
		t.Errorf("approval ticket type = %q, want %q", tickets[0].Type, approval.TicketHardLoop)
	}
	if !hasAuditAction(audit.Events(), "approval_opened") {
		t.Errorf("audit actions missing %q: %#v", "approval_opened", audit.Events())
	}
	if !hasLearningRuntimeEvent(events.Events(), evolution.SignalHardLoopFailure) {
		t.Errorf("runtime events missing hard loop learning event: %#v", events.Events())
	}
}

func TestCoordinatorRoutesTaskToRegisteredAgentRunner(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	scheduler := task.NewScheduler()
	if err := scheduler.Add(ctx, domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		AgentID:   "researcher",
		Status:    domain.TaskPending,
		Input:     "research this",
	}); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}
	runner := &recordingTaskRunner{result: "research complete"}
	resolver := &staticTaskRunnerResolver{
		agent: domain.Agent{
			ID:        "researcher",
			CompanyID: "company-1",
			Role:      "researcher",
			Status:    domain.AgentActive,
		},
		runner: runner,
		ok:     true,
	}
	coordinator := NewCoordinator(CoordinatorConfig{
		Agent: domain.Agent{
			ID:        "coordinator",
			CompanyID: "company-1",
			Role:      "coordinator",
			Status:    domain.AgentActive,
		},
		Scheduler:          scheduler,
		Locks:              task.NewLockStore(),
		Runtime:            &recordingTaskRunner{result: "default complete"},
		TaskRunnerResolver: resolver,
		Reviewer:           quality.NewAegisReviewer(),
		Evaluator:          quality.NewEvalEngine(3),
		Approvals:          approval.NewService(),
		LockTTL:            time.Minute,
	})

	current, ok, err := coordinator.Heartbeat(ctx)
	if err != nil {
		t.Fatalf("Heartbeat() error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("Heartbeat() ok = false, want true")
	}
	if current.AgentID != "researcher" {
		t.Errorf("Heartbeat() current task AgentID = %q, want %q", current.AgentID, "researcher")
	}
	if runner.lastRun.AgentID != "researcher" {
		t.Errorf("Heartbeat() run AgentID = %q, want %q", runner.lastRun.AgentID, "researcher")
	}
	if runner.lastRun.Result != "research complete" {
		t.Errorf("Heartbeat() run result = %q, want %q", runner.lastRun.Result, "research complete")
	}
	if runner.lastTask.AgentID != "researcher" {
		t.Errorf("Heartbeat() runner task AgentID = %q, want %q", runner.lastTask.AgentID, "researcher")
	}
	if resolver.lastTask.ID != "task-1" {
		t.Errorf("ResolveTaskRunner() task ID = %q, want %q", resolver.lastTask.ID, "task-1")
	}
}

func TestCoordinatorPublishesHardLoopLearningForResolvedAgent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	scheduler := task.NewScheduler()
	if err := scheduler.Add(ctx, domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		AgentID:   "researcher",
		Status:    domain.TaskPending,
		Input:     "loop",
	}); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}
	events := adapter.NewMemoryEventBus()
	coordinator := NewCoordinator(CoordinatorConfig{
		Agent: domain.Agent{
			ID:        "coordinator",
			CompanyID: "company-1",
			Role:      "coordinator",
			Status:    domain.AgentActive,
		},
		Scheduler: scheduler,
		Locks:     task.NewLockStore(),
		Runtime:   &recordingTaskRunner{result: "default"},
		TaskRunnerResolver: &staticTaskRunnerResolver{
			agent: domain.Agent{
				ID:        "researcher",
				CompanyID: "company-1",
				Role:      "researcher",
				Status:    domain.AgentActive,
			},
			runner: &recordingTaskRunner{result: "same"},
			ok:     true,
		},
		Reviewer:  quality.NewAegisReviewer(),
		Evaluator: quality.NewEvalEngine(2),
		Approvals: approval.NewService(),
		Events:    events,
		LockTTL:   time.Minute,
	})

	current, ok, err := coordinator.Heartbeat(ctx)
	if err != nil {
		t.Fatalf("Heartbeat() error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("Heartbeat() ok = false, want true")
	}
	if current.Status != domain.TaskSuspended {
		t.Fatalf("Heartbeat() status = %q, want %q", current.Status, domain.TaskSuspended)
	}
	if !hasLearningRuntimeEventForAgent(events.Events(), "researcher", evolution.SignalHardLoopFailure) {
		t.Fatalf("runtime events missing researcher hard loop learning event: %#v", events.Events())
	}
}

func TestCoordinatorFallsBackToDefaultRuntimeWhenResolverMisses(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	scheduler := task.NewScheduler()
	if err := scheduler.Add(ctx, domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Status:    domain.TaskPending,
		Input:     "ship it",
	}); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}
	runner := &recordingTaskRunner{result: "default complete"}
	resolver := &staticTaskRunnerResolver{ok: false}
	coordinator := NewCoordinator(CoordinatorConfig{
		Agent: domain.Agent{
			ID:        "agent-1",
			CompanyID: "company-1",
			Role:      "developer",
			Status:    domain.AgentActive,
		},
		Scheduler:          scheduler,
		Locks:              task.NewLockStore(),
		Runtime:            runner,
		TaskRunnerResolver: resolver,
		Reviewer:           quality.NewAegisReviewer(),
		Evaluator:          quality.NewEvalEngine(3),
		Approvals:          approval.NewService(),
		LockTTL:            time.Minute,
	})

	current, ok, err := coordinator.Heartbeat(ctx)
	if err != nil {
		t.Fatalf("Heartbeat() error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("Heartbeat() ok = false, want true")
	}
	if current.Status != domain.TaskDone {
		t.Errorf("Heartbeat() status = %q, want %q", current.Status, domain.TaskDone)
	}
	if runner.lastRun.AgentID != "agent-1" {
		t.Errorf("Heartbeat() fallback run AgentID = %q, want %q", runner.lastRun.AgentID, "agent-1")
	}
	if runner.lastRun.Result != "default complete" {
		t.Errorf("Heartbeat() fallback run result = %q, want %q", runner.lastRun.Result, "default complete")
	}
	if resolver.calls != 1 {
		t.Errorf("ResolveTaskRunner() calls = %d, want 1", resolver.calls)
	}
}

func TestCoordinatorFailsTaskWhenResolvedRunnerIsNil(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	scheduler := task.NewScheduler()
	if err := scheduler.Add(ctx, domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		AgentID:   "researcher",
		Status:    domain.TaskPending,
		Input:     "research",
	}); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}
	coordinator := NewCoordinator(CoordinatorConfig{
		Agent: domain.Agent{
			ID:        "coordinator",
			CompanyID: "company-1",
			Role:      "coordinator",
			Status:    domain.AgentActive,
		},
		Scheduler:          scheduler,
		Locks:              task.NewLockStore(),
		Runtime:            &recordingTaskRunner{result: "default"},
		TaskRunnerResolver: &staticTaskRunnerResolver{ok: true},
		Reviewer:           quality.NewAegisReviewer(),
		Evaluator:          quality.NewEvalEngine(3),
		Approvals:          approval.NewService(),
		LockTTL:            time.Minute,
	})

	_, ok, err := coordinator.Heartbeat(ctx)
	if err == nil {
		t.Fatalf("Heartbeat() error = nil, want nil runner error")
	}
	if ok {
		t.Fatalf("Heartbeat() ok = true, want false")
	}
	if !strings.Contains(err.Error(), "runner is nil") {
		t.Fatalf("Heartbeat() error = %v, want runner is nil", err)
	}
	current, _, getErr := scheduler.Get(ctx, "task-1")
	if getErr != nil {
		t.Fatalf("Get(task-1) error = %v, want nil", getErr)
	}
	if current.Status != domain.TaskFailed {
		t.Fatalf("task status = %q, want %q", current.Status, domain.TaskFailed)
	}
}

func hasAuditAction(events []domain.AuditEvent, action string) bool {
	for _, event := range events {
		if event.Action == action {
			return true
		}
	}
	return false
}

func hasLearningRuntimeEventForAgent(events []domain.RuntimeEvent, agentID string, signal evolution.SignalKind) bool {
	for _, event := range events {
		learning, ok := evolution.ParseLearningRuntimeEvent(event)
		if !ok {
			continue
		}
		if learning.AgentID == agentID && learning.Signal == signal {
			return true
		}
	}
	return false
}

type recordingTaskRunner struct {
	result    string
	calls     int
	lastAgent domain.Agent
	lastTask  domain.Task
	lastRun   domain.TaskRun
}

func (r *recordingTaskRunner) RunTask(ctx context.Context, agent domain.Agent, task domain.Task) (domain.TaskRun, error) {
	if err := ctx.Err(); err != nil {
		return domain.TaskRun{}, err
	}
	r.calls++
	r.lastAgent = agent
	r.lastTask = task
	r.lastRun = domain.TaskRun{
		ID:      task.ID + ":run",
		TaskID:  task.ID,
		AgentID: agent.ID,
		Result:  r.result,
	}
	return r.lastRun, nil
}

type staticTaskRunnerResolver struct {
	agent    domain.Agent
	runner   TaskRunner
	ok       bool
	err      error
	calls    int
	lastTask domain.Task
}

func (r *staticTaskRunnerResolver) ResolveTaskRunner(ctx context.Context, task domain.Task) (domain.Agent, TaskRunner, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Agent{}, nil, false, err
	}
	if r.err != nil {
		return domain.Agent{}, nil, false, r.err
	}
	r.calls++
	r.lastTask = task
	return r.agent, r.runner, r.ok, nil
}

var errStaticResolver = errors.New("static resolver error")
