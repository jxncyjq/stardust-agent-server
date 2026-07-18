package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/sessionstate"
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
	// MaxWorkers caps concurrent task goroutines. 0 or negative → default 4.
	MaxWorkers int
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
	sem                chan struct{}
	wg                 sync.WaitGroup
}

func NewCoordinator(cfg CoordinatorConfig) *Coordinator {
	if cfg.LockTTL == 0 {
		cfg.LockTTL = time.Minute
	}
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 4
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
		sem:                make(chan struct{}, cfg.MaxWorkers),
	}
}

// Heartbeat dispatches as many pending tasks as there are free worker slots,
// each on its own goroutine, then returns immediately. A slow or suspended task
// no longer blocks others. The returned Task is always zero-valued now (work is
// async); the bool reports whether at least one task was dispatched this tick.
func (c *Coordinator) Heartbeat(ctx context.Context) (domain.Task, bool, error) {
	dispatched := false
	for {
		select {
		case c.sem <- struct{}{}: // acquired a worker slot
		default:
			return domain.Task{}, dispatched, nil // all workers busy
		}
		taskToRun, ok, err := c.scheduler.Next(ctx, c.agent.ID)
		if err != nil {
			<-c.sem
			return domain.Task{}, dispatched, fmt.Errorf("schedule next task: %w", err)
		}
		if !ok {
			<-c.sem // no pending task; release the slot
			return domain.Task{}, dispatched, nil
		}
		c.wg.Add(1)
		go func(t domain.Task) {
			defer c.wg.Done()
			defer func() { <-c.sem }()
			if _, _, err := c.runAssigned(ctx, t); err != nil {
				// Goroutine top-level: never swallow. runAssigned already
				// transitioned the task to Failed on error; record the reason so
				// a failed run is diagnosable rather than vanishing.
				_ = c.publishLearning(ctx, c.agent.ID, t.ID, evolution.SignalFailure, "task_run_error", true)
			}
		}(taskToRun)
		dispatched = true
	}
}

// Wait blocks until every in-flight task goroutine has finished. The serve
// shutdown path calls it so tasks are not abandoned mid-run.
func (c *Coordinator) Wait() {
	c.wg.Wait()
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
		if errors.Is(err, ErrSuspended) {
			// The runtime checkpointed and paused (e.g. awaiting approval). Land
			// the task in Suspended — NOT Failed — and release the goroutine. A
			// later decision (or restart recovery) transitions it back to Running
			// and the runtime auto-resumes from its checkpoint.
			if txErr := c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskSuspended); txErr != nil {
				return domain.Task{}, false, fmt.Errorf("suspend checkpointed task %s: %w", taskToRun.ID, txErr)
			}
			if auErr := c.appendAudit(ctx, taskToRun.ID, "task_suspended"); auErr != nil {
				return domain.Task{}, false, auErr
			}
			return c.currentTask(ctx, taskToRun.ID)
		}
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

// RecoverSuspended re-registers every task that has a persisted checkpoint into
// the scheduler in TaskSuspended state, so suspends survive a process restart and
// remain visible/decidable. It does not resume them — resume is driven by a
// Suspended→Running decision. Returns the number of tasks recovered. A task that
// is already present in the scheduler is skipped (idempotent re-scan).
func (c *Coordinator) RecoverSuspended(ctx context.Context, store *sessionstate.Store) (int, error) {
	if store == nil {
		return 0, nil
	}
	checkpoints, err := store.ListSuspended()
	if err != nil {
		return 0, fmt.Errorf("list suspended checkpoints: %w", err)
	}
	recovered := 0
	for _, cp := range checkpoints {
		if _, ok, err := c.scheduler.Get(ctx, cp.TaskID); err != nil {
			return recovered, fmt.Errorf("check task %s presence: %w", cp.TaskID, err)
		} else if ok {
			continue
		}
		if err := c.scheduler.Add(ctx, domain.Task{
			ID:        cp.TaskID,
			AgentID:   cp.AgentID,
			SessionID: cp.SessionKey,
			Status:    domain.TaskSuspended,
		}); err != nil {
			return recovered, fmt.Errorf("re-register suspended task %s: %w", cp.TaskID, err)
		}
		if err := c.appendAudit(ctx, cp.TaskID, "task_recovered_suspended"); err != nil {
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
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
