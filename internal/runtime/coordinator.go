package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/task"
)

type TaskRunner interface {
	RunTask(context.Context, domain.Agent, domain.Task) (domain.TaskRun, error)
}

type TaskRunnerResolver interface {
	ResolveTaskRunner(context.Context, domain.Task) (domain.Agent, TaskRunner, bool, error)
}

// TrustGate decides whether an agent is currently trusted enough to execute a
// task. It is satisfied by quality.TrustScoreManager. A nil gate means trust
// gating is disabled (a valid "trust not configured" deployment), not an error.
type TrustGate interface {
	CanExecute(ctx context.Context, agentID string, risk quality.RiskLevel, at time.Time) (quality.TrustDecision, error)
}

type CoordinatorConfig struct {
	Agent              domain.Agent
	Scheduler          *task.Scheduler
	Locks              *task.LockStore
	Runtime            TaskRunner
	TaskRunnerResolver TaskRunnerResolver
	Reviewer           quality.AegisReviewer
	Evaluator          quality.EvalEngine
	Approvals          *approval.Service
	Audit              port.AuditLog
	Events             port.EventBus
	TrustGate          TrustGate
	LockTTL            time.Duration
}

type Coordinator struct {
	agent              domain.Agent
	scheduler          *task.Scheduler
	locks              *task.LockStore
	runtime            TaskRunner
	taskRunnerResolver TaskRunnerResolver
	reviewer           quality.AegisReviewer
	evaluator          quality.EvalEngine
	approvals          *approval.Service
	audit              port.AuditLog
	events             port.EventBus
	trustGate          TrustGate
	lockTTL            time.Duration
}

func NewCoordinator(cfg CoordinatorConfig) *Coordinator {
	if cfg.LockTTL == 0 {
		cfg.LockTTL = time.Minute
	}
	return &Coordinator{
		agent:              cfg.Agent,
		scheduler:          cfg.Scheduler,
		locks:              cfg.Locks,
		runtime:            cfg.Runtime,
		taskRunnerResolver: cfg.TaskRunnerResolver,
		reviewer:           cfg.Reviewer,
		evaluator:          cfg.Evaluator,
		approvals:          cfg.Approvals,
		audit:              cfg.Audit,
		events:             cfg.Events,
		trustGate:          cfg.TrustGate,
		lockTTL:            cfg.LockTTL,
	}
}

func (c *Coordinator) Heartbeat(ctx context.Context) (domain.Task, bool, error) {
	taskToRun, ok, err := c.scheduler.Next(ctx, c.agent.ID)
	if err != nil {
		return domain.Task{}, false, fmt.Errorf("schedule next task: %w", err)
	}
	if !ok {
		return domain.Task{}, false, nil
	}
	return c.runAssigned(ctx, taskToRun)
}

// runAssigned executes the full pipeline for a task the scheduler has already
// handed out: acquire its lock, mark it running, resolve its runner, run it,
// then evaluate/review and land it in a terminal (or suspended) state. It is the
// unit spawned per-task by Heartbeat.
func (c *Coordinator) runAssigned(ctx context.Context, taskToRun domain.Task) (domain.Task, bool, error) {
	locked, err := c.locks.TryLock(ctx, taskToRun.ID, c.agent.ID, c.lockTTL)
	if err != nil {
		return domain.Task{}, false, fmt.Errorf("lock task: %w", err)
	}
	if !locked {
		return domain.Task{}, false, nil
	}
	if err := c.appendAudit(ctx, taskToRun.ID, "task_assigned"); err != nil {
		return domain.Task{}, false, err
	}
	if err := c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskRunning); err != nil {
		return domain.Task{}, false, fmt.Errorf("mark task running: %w", err)
	}
	if err := c.appendAudit(ctx, taskToRun.ID, "task_running"); err != nil {
		return domain.Task{}, false, err
	}
	runnerAgent := c.agent
	runner := c.runtime
	if c.taskRunnerResolver != nil {
		resolvedAgent, resolvedRunner, resolved, err := c.taskRunnerResolver.ResolveTaskRunner(ctx, taskToRun)
		if err != nil {
			_ = c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskFailed)
			return domain.Task{}, false, fmt.Errorf("resolve task runner for task %s: %w", taskToRun.ID, err)
		}
		if resolved {
			if resolvedRunner == nil {
				_ = c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskFailed)
				return domain.Task{}, false, fmt.Errorf("resolve task runner for task %s: runner is nil", taskToRun.ID)
			}
			runnerAgent = resolvedAgent
			runner = resolvedRunner
		}
	}
	if runner == nil {
		_ = c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskFailed)
		return domain.Task{}, false, fmt.Errorf("run task %s: runtime is nil", taskToRun.ID)
	}
	if c.trustGate != nil {
		decision, err := c.trustGate.CanExecute(ctx, runnerAgent.ID, quality.RiskLow, time.Now())
		if err != nil {
			_ = c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskFailed)
			return domain.Task{}, false, fmt.Errorf("evaluate trust gate for task %s: %w", taskToRun.ID, err)
		}
		if decision == quality.TrustDecisionBlocked {
			// Distrusted agent: suspend the task for human review instead of
			// running it, and record why (fail-loud — never silently drop).
			if err := c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskSuspended); err != nil {
				return domain.Task{}, false, fmt.Errorf("suspend trust-blocked task %s: %w", taskToRun.ID, err)
			}
			if err := c.appendAudit(ctx, taskToRun.ID, "trust_blocked"); err != nil {
				return domain.Task{}, false, err
			}
			if err := c.publishLearning(ctx, runnerAgent.ID, taskToRun.ID, evolution.SignalPermissionViolation, "trust_blocked", false); err != nil {
				return domain.Task{}, false, err
			}
			return c.currentTask(ctx, taskToRun.ID)
		}
	}
	run, err := runner.RunTask(ctx, runnerAgent, taskToRun)
	if err != nil {
		_ = c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskFailed)
		return domain.Task{}, false, fmt.Errorf("run task: %w", err)
	}
	eval, err := c.evaluator.EvaluateTrace(ctx, []string{run.Result, run.Result})
	if err != nil {
		return domain.Task{}, false, fmt.Errorf("evaluate task trace: %w", err)
	}
	if eval.Status == quality.EvalHardLoop {
		if err := c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskSuspended); err != nil {
			return domain.Task{}, false, fmt.Errorf("suspend hard loop task: %w", err)
		}
		if err := c.appendAudit(ctx, taskToRun.ID, "hard_loop_suspended"); err != nil {
			return domain.Task{}, false, err
		}
		if _, err := c.approvals.OpenTicket(ctx, approval.OpenTicketRequest{
			Type:      approval.TicketHardLoop,
			SubjectID: taskToRun.ID,
			Reason:    eval.Reason,
		}); err != nil {
			return domain.Task{}, false, fmt.Errorf("open hard loop approval ticket: %w", err)
		}
		if err := c.appendAudit(ctx, taskToRun.ID, "approval_opened"); err != nil {
			return domain.Task{}, false, err
		}
		if err := c.publishLearning(ctx, runnerAgent.ID, taskToRun.ID, evolution.SignalHardLoopFailure, evolution.FailureReasonHardLoop, false); err != nil {
			return domain.Task{}, false, err
		}
		return c.currentTask(ctx, taskToRun.ID)
	}
	if err := c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskQualityReview); err != nil {
		return domain.Task{}, false, fmt.Errorf("mark task quality review: %w", err)
	}
	review, err := c.reviewer.Review(ctx, run)
	if err != nil {
		return domain.Task{}, false, fmt.Errorf("review task result: %w", err)
	}
	if !review.Approved {
		if err := c.appendAudit(ctx, taskToRun.ID, "quality_rejected"); err != nil {
			return domain.Task{}, false, err
		}
		if err := c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskFailed); err != nil {
			return domain.Task{}, false, fmt.Errorf("mark rejected task failed: %w", err)
		}
		return c.currentTask(ctx, taskToRun.ID)
	}
	if err := c.appendAudit(ctx, taskToRun.ID, "quality_approved"); err != nil {
		return domain.Task{}, false, err
	}
	if err := c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskDone); err != nil {
		return domain.Task{}, false, fmt.Errorf("mark task done: %w", err)
	}
	if err := c.appendAudit(ctx, taskToRun.ID, "task_done"); err != nil {
		return domain.Task{}, false, err
	}
	return c.currentTask(ctx, taskToRun.ID)
}

func (c *Coordinator) currentTask(ctx context.Context, taskID string) (domain.Task, bool, error) {
	current, ok, err := c.scheduler.Get(ctx, taskID)
	if err != nil {
		return domain.Task{}, false, err
	}
	return current, ok, nil
}

func (c *Coordinator) appendAudit(ctx context.Context, taskID string, action string) error {
	if c.audit == nil {
		return nil
	}
	if err := c.audit.Append(ctx, domain.AuditEvent{
		ID:          taskID + ":" + action,
		RequestID:   taskID + ":coordinator",
		SubjectType: "task",
		SubjectID:   taskID,
		Action:      action,
		Hash:        "memory",
		CreatedAt:   time.Now(),
	}); err != nil {
		return fmt.Errorf("append %s audit event: %w", action, err)
	}
	return nil
}

func (c *Coordinator) publishLearning(ctx context.Context, agentID string, taskID string, signal evolution.SignalKind, reason string, lightweight bool) error {
	if c.events == nil {
		return nil
	}
	if err := c.events.Publish(ctx, evolution.NewLearningRuntimeEvent(evolution.LearningEvent{
		AgentID:       agentID,
		TaskID:        taskID,
		Signal:        signal,
		Reason:        reason,
		IsLightweight: lightweight,
		PublishedAt:   time.Now(),
	})); err != nil {
		return fmt.Errorf("publish learning event: %w", err)
	}
	return nil
}
