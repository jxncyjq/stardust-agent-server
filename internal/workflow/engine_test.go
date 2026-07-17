package workflow

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/task"
)

func TestEngineRunsSequenceAgentTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheduler := task.NewScheduler()
	events := adapter.NewMemoryEventBus()
	audit := adapter.NewMemoryAuditLog()
	engine := NewEngine(Config{
		Scheduler: scheduler,
		Approvals: approval.NewService(),
		Events:    events,
		Audit:     audit,
	})

	result, err := engine.Execute(ctx, Definition{
		ID: "workflow-1",
		Root: Node{
			ID:   "sequence-1",
			Kind: NodeSequence,
			Children: []Node{
				{ID: "task-node-1", Kind: NodeAgentTask, Task: TaskSpec{ID: "task-1", CompanyID: "company-1", Input: "first"}},
				{ID: "task-node-2", Kind: NodeAgentTask, Task: TaskSpec{ID: "task-2", CompanyID: "company-1", Input: "second"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute(sequence) error = %v, want nil", err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("Execute(sequence) status = %s, want %s", result.Status, StatusCompleted)
	}
	for _, taskID := range []string{"task-1", "task-2"} {
		got, ok, err := scheduler.Get(ctx, taskID)
		if err != nil {
			t.Fatalf("Get(%q) error = %v, want nil", taskID, err)
		}
		if !ok {
			t.Fatalf("Get(%q) ok = false, want true", taskID)
		}
		if got.Status != domain.TaskPending {
			t.Errorf("Get(%q) status = %s, want %s", taskID, got.Status, domain.TaskPending)
		}
	}
	if !hasWorkflowEvent(events.Events(), "workflow_completed") {
		t.Errorf("events missing workflow_completed: %#v", events.Events())
	}
	if !hasWorkflowAudit(audit.Events(), "workflow_completed") {
		t.Errorf("audit missing workflow_completed: %#v", audit.Events())
	}
}

func TestEngineRendersCompletedTaskResultIntoLaterTaskInput(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	scheduler := task.NewScheduler()
	events := adapter.NewMemoryEventBus()
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:    "task_completed",
		TaskID:  "research-task",
		Message: "cache uses LRU eviction",
	}); err != nil {
		t.Fatalf("Publish(task_completed) error = %v, want nil", err)
	}
	engine := NewEngine(Config{
		Scheduler: scheduler,
		Approvals: approval.NewService(),
		Events:    events,
		Audit:     adapter.NewMemoryAuditLog(),
	})

	result, err := engine.Execute(ctx, Definition{
		ID: "workflow-result-handoff",
		Root: Node{
			ID:   "sequence-1",
			Kind: NodeSequence,
			Children: []Node{
				{
					ID:          "wait-research",
					Kind:        NodeWaitEvent,
					EventType:   "task_completed",
					EventTaskID: "research-task",
				},
				{
					ID:   "writer-node",
					Kind: NodeAgentTask,
					Task: TaskSpec{
						ID:        "writer-task",
						CompanyID: "company-1",
						AgentID:   "writer",
						Input:     "整理前序结果：{{tasks.research-task.result}}",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute(result handoff) error = %v, want nil", err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("Execute(result handoff) status = %s, want %s", result.Status, StatusCompleted)
	}
	got, ok, err := scheduler.Get(ctx, "writer-task")
	if err != nil {
		t.Fatalf("Get(writer-task) error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("Get(writer-task) ok = false, want true")
	}
	if got.Input != "整理前序结果：cache uses LRU eviction" {
		t.Fatalf("writer-task input = %q, want rendered task result", got.Input)
	}
}

func TestEngineApprovalNodePausesWorkflow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	approvals := approval.NewService()
	engine := NewEngine(Config{
		Scheduler: task.NewScheduler(),
		Approvals: approvals,
		Events:    adapter.NewMemoryEventBus(),
		Audit:     adapter.NewMemoryAuditLog(),
	})

	result, err := engine.Execute(ctx, Definition{
		ID: "workflow-approval",
		Root: Node{
			ID:      "approval-1",
			Kind:    NodeApproval,
			Subject: "workflow-approval",
			Reason:  "manual gate",
		},
	})
	if err != nil {
		t.Fatalf("Execute(approval) error = %v, want nil", err)
	}
	if result.Status != StatusWaitingApproval {
		t.Fatalf("Execute(approval) status = %s, want %s", result.Status, StatusWaitingApproval)
	}
	tickets := approvals.Tickets()
	if len(tickets) != 1 {
		t.Fatalf("Tickets() len = %d, want 1", len(tickets))
	}
	if tickets[0].Type != approval.TicketWorkflowHumanGate {
		t.Fatalf("Tickets()[0].Type = %s, want %s", tickets[0].Type, approval.TicketWorkflowHumanGate)
	}
}

func TestEngineWaitEventCompletesWhenEventAlreadyExists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:    "external.ready",
		TaskID:  "task-42",
		Message: "payload is ready",
	}); err != nil {
		t.Fatalf("Publish(external.ready) error = %v, want nil", err)
	}
	engine := NewEngine(Config{
		Scheduler: task.NewScheduler(),
		Approvals: approval.NewService(),
		Events:    events,
		Audit:     adapter.NewMemoryAuditLog(),
	})

	result, err := engine.Execute(ctx, Definition{
		ID: "workflow-wait-ready",
		Root: Node{
			ID:                   "wait-ready",
			Kind:                 NodeWaitEvent,
			EventType:            "external.ready",
			EventTaskID:          "task-42",
			EventMessageContains: "ready",
		},
	})
	if err != nil {
		t.Fatalf("Execute(wait_event ready) error = %v, want nil", err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("Execute(wait_event ready) status = %s, want %s", result.Status, StatusCompleted)
	}
	if !hasNodeStatus(result.Nodes, "wait-ready", StatusCompleted) {
		t.Fatalf("Execute(wait_event ready) nodes = %#v, want wait-ready completed", result.Nodes)
	}
}

func TestEngineWaitEventPausesWhenEventIsMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	engine := NewEngine(Config{
		Scheduler: task.NewScheduler(),
		Approvals: approval.NewService(),
		Events:    events,
		Audit:     adapter.NewMemoryAuditLog(),
	})

	result, err := engine.Execute(ctx, Definition{
		ID: "workflow-wait-missing",
		Root: Node{
			ID:        "wait-missing",
			Kind:      NodeWaitEvent,
			EventType: "external.ready",
		},
	})
	if err != nil {
		t.Fatalf("Execute(wait_event missing) error = %v, want nil", err)
	}
	if result.Status != StatusWaitingEvent {
		t.Fatalf("Execute(wait_event missing) status = %s, want %s", result.Status, StatusWaitingEvent)
	}
	if !hasNodeStatus(result.Nodes, "wait-missing", StatusWaitingEvent) {
		t.Fatalf("Execute(wait_event missing) nodes = %#v, want wait-missing waiting_event", result.Nodes)
	}
	if !hasWorkflowEvent(events.Events(), "workflow_waiting_event") {
		t.Fatalf("events missing workflow_waiting_event: %#v", events.Events())
	}
}

func TestEngineSubworkflowRunsNestedDefinition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheduler := task.NewScheduler()
	engine := NewEngine(Config{
		Scheduler: scheduler,
		Approvals: approval.NewService(),
		Events:    adapter.NewMemoryEventBus(),
		Audit:     adapter.NewMemoryAuditLog(),
	})

	result, err := engine.Execute(ctx, Definition{
		ID: "workflow-parent",
		Root: Node{
			ID:   "child-workflow-node",
			Kind: NodeSubworkflow,
			Subworkflow: &Definition{
				ID: "workflow-child",
				Root: Node{
					ID:   "child-task-node",
					Kind: NodeAgentTask,
					Task: TaskSpec{ID: "child-task", CompanyID: "company-1", Input: "nested"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute(subworkflow) error = %v, want nil", err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("Execute(subworkflow) status = %s, want %s", result.Status, StatusCompleted)
	}
	if _, ok, _ := scheduler.Get(ctx, "child-task"); !ok {
		t.Fatalf("Get(child-task) ok = false, want true")
	}
	if !hasNodeStatus(result.Nodes, "child-task-node", StatusCompleted) {
		t.Fatalf("Execute(subworkflow) nodes = %#v, want child-task-node completed", result.Nodes)
	}
	if !hasNodeStatus(result.Nodes, "child-workflow-node", StatusCompleted) {
		t.Fatalf("Execute(subworkflow) nodes = %#v, want child-workflow-node completed", result.Nodes)
	}
}

func TestEngineConditionChoosesBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheduler := task.NewScheduler()
	engine := NewEngine(Config{
		Scheduler: scheduler,
		Approvals: approval.NewService(),
		Events:    adapter.NewMemoryEventBus(),
		Audit:     adapter.NewMemoryAuditLog(),
	})

	result, err := engine.Execute(ctx, Definition{
		ID:        "workflow-condition",
		Variables: map[string]string{"route": "yes"},
		Root: Node{
			ID:             "condition-1",
			Kind:           NodeCondition,
			ConditionKey:   "route",
			ConditionValue: "yes",
			Then:           &Node{ID: "then-task", Kind: NodeAgentTask, Task: TaskSpec{ID: "then", CompanyID: "company-1", Input: "then"}},
			Else:           &Node{ID: "else-task", Kind: NodeAgentTask, Task: TaskSpec{ID: "else", CompanyID: "company-1", Input: "else"}},
		},
	})
	if err != nil {
		t.Fatalf("Execute(condition) error = %v, want nil", err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("Execute(condition) status = %s, want %s", result.Status, StatusCompleted)
	}
	if _, ok, _ := scheduler.Get(ctx, "then"); !ok {
		t.Fatalf("Get(then) ok = false, want true")
	}
	if _, ok, _ := scheduler.Get(ctx, "else"); ok {
		t.Fatalf("Get(else) ok = true, want false")
	}
}

func TestEngineErrorHandlerRunsCompensation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheduler := task.NewScheduler()
	engine := NewEngine(Config{
		Scheduler: scheduler,
		Approvals: approval.NewService(),
		Events:    adapter.NewMemoryEventBus(),
		Audit:     adapter.NewMemoryAuditLog(),
	})

	result, err := engine.Execute(ctx, Definition{
		ID: "workflow-error-handler",
		Root: Node{
			ID:      "handler-1",
			Kind:    NodeErrorHandler,
			Try:     &Node{ID: "bad-task", Kind: NodeAgentTask, Task: TaskSpec{Input: "missing id"}},
			Handler: &Node{ID: "compensate", Kind: NodeAgentTask, Task: TaskSpec{ID: "compensated", CompanyID: "company-1", Input: "compensate"}},
		},
	})
	if err != nil {
		t.Fatalf("Execute(error_handler) error = %v, want nil", err)
	}
	if result.Status != StatusCompensated {
		t.Fatalf("Execute(error_handler) status = %s, want %s", result.Status, StatusCompensated)
	}
	if _, ok, _ := scheduler.Get(ctx, "compensated"); !ok {
		t.Fatalf("Get(compensated) ok = false, want true")
	}
}

func TestEngineParallelFailFastFailsWorkflow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine := NewEngine(Config{
		Scheduler: task.NewScheduler(),
		Approvals: approval.NewService(),
		Events:    adapter.NewMemoryEventBus(),
		Audit:     adapter.NewMemoryAuditLog(),
	})

	result, err := engine.Execute(ctx, Definition{
		ID: "workflow-parallel",
		Root: Node{
			ID:            "parallel-1",
			Kind:          NodeParallel,
			FailurePolicy: FailurePolicyFailFast,
			Children: []Node{
				{ID: "bad-task", Kind: NodeAgentTask, Task: TaskSpec{Input: "missing id"}},
				{ID: "good-task", Kind: NodeAgentTask, Task: TaskSpec{ID: "task-1", CompanyID: "company-1", Input: "ok"}},
			},
		},
	})
	if err == nil {
		t.Fatalf("Execute(parallel fail_fast) error = nil, want error")
	}
	if result.Status != StatusFailed {
		t.Fatalf("Execute(parallel fail_fast) status = %s, want %s", result.Status, StatusFailed)
	}
}

func TestWorkflowLoopJoinQuorum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheduler := task.NewScheduler()
	engine := NewEngine(Config{
		Scheduler: scheduler,
		Approvals: approval.NewService(),
		Events:    adapter.NewMemoryEventBus(),
		Audit:     adapter.NewMemoryAuditLog(),
	})

	result, err := engine.Execute(ctx, Definition{
		ID: "workflow-loop-join",
		Root: Node{
			ID:   "root-sequence",
			Kind: NodeSequence,
			Children: []Node{
				{
					ID:                "loop-1",
					Kind:              NodeLoop,
					LoopMaxIterations: 2,
					LoopBody: &Node{
						ID:   "loop-task-{{iteration}}",
						Kind: NodeAgentTask,
						Task: TaskSpec{ID: "loop-task-{{iteration}}", CompanyID: "company-1", Input: "loop {{iteration}}"},
					},
				},
				{
					ID:     "join-1",
					Kind:   NodeJoin,
					Quorum: 2,
					Children: []Node{
						{ID: "join-good-1", Kind: NodeAgentTask, Task: TaskSpec{ID: "join-task-1", CompanyID: "company-1", Input: "one"}},
						{ID: "join-good-2", Kind: NodeAgentTask, Task: TaskSpec{ID: "join-task-2", CompanyID: "company-1", Input: "two"}},
						{ID: "join-bad", Kind: NodeAgentTask, Task: TaskSpec{Input: "missing id"}},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute(loop join quorum) error = %v, want nil", err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("Execute(loop join quorum) status = %s, want %s", result.Status, StatusCompleted)
	}
	for _, taskID := range []string{"loop-task-1", "loop-task-2", "join-task-1", "join-task-2"} {
		if _, ok, err := scheduler.Get(ctx, taskID); err != nil || !ok {
			t.Fatalf("Scheduler.Get(%q) = ok %t error %v, want created task", taskID, ok, err)
		}
	}
	if !hasNodeStatus(result.Nodes, "loop-1", StatusCompleted) {
		t.Fatalf("Execute(loop join quorum) nodes = %#v, want loop completed", result.Nodes)
	}
	if !hasNodeStatus(result.Nodes, "join-1", StatusCompleted) {
		t.Fatalf("Execute(loop join quorum) nodes = %#v, want join completed", result.Nodes)
	}
}

func hasWorkflowEvent(events []domain.RuntimeEvent, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func hasWorkflowAudit(events []domain.AuditEvent, action string) bool {
	for _, event := range events {
		if event.Action == action {
			return true
		}
	}
	return false
}

func hasNodeStatus(nodes []NodeResult, nodeID string, status Status) bool {
	for _, node := range nodes {
		if node.NodeID == nodeID && node.Status == status {
			return true
		}
	}
	return false
}
