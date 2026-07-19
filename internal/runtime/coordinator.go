package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// RuntimeEventTaskFailed carries the underlying cause of a failed task run.
// It exists separately from the failure learning event because that event's
// reason field is a fixed vocabulary (evolution.FailureReason*) parsed out of a
// whitespace-separated key=value message: an error string, which always
// contains spaces, would be truncated to its first word and would corrupt the
// fields after it. The cause therefore travels here, unabridged.
const RuntimeEventTaskFailed = "task_failed"

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
	// Checkpoints, when set, enables Heartbeat's resume scan: a Running task
	// with a persisted checkpoint (flipped Suspended→Running by a human
	// decision) is re-dispatched from where it left off. Nil disables the scan
	// (a valid "no manual-mode resume support configured" deployment).
	Checkpoints *sessionstate.Store
	// Logger records task-run failures structurally at the per-task goroutine
	// boundary, where there is no caller left to return an error to. Nil is a
	// valid "no logging configured" deployment (tests, embedded use): the
	// failure is still published as a RuntimeEventTaskFailed event, so the
	// cause is never lost outright.
	Logger *slog.Logger
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
	checkpoints        *sessionstate.Store
	logger             *slog.Logger
	sem                chan struct{}
	wg                 sync.WaitGroup

	// resumingMu/resuming guard against double-dispatch of the same task's
	// resume path within THIS process. TryLock's lease (lockTTL) is a fixed
	// duration that is never renewed while a resume is in flight; a run that
	// outlives the lease (routine for a real LLM agent) leaves the task
	// Running with its checkpoint still on disk, so the next resumeScan tick
	// would re-TryLock (now free) and start a second concurrent runResume of
	// the same task. This in-process set is not lease-bound: it is held for
	// the entire runResume call, independent of the lock's TTL. The lock
	// still matters for cross-process/restart safety; this map is the
	// within-process guard that closes the gap the lock alone leaves open.
	resumingMu sync.Mutex
	resuming   map[string]bool
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
		checkpoints:        cfg.Checkpoints,
		logger:             cfg.Logger,
		sem:                make(chan struct{}, cfg.MaxWorkers),
		resuming:           make(map[string]bool),
	}
}

// tryMarkResuming atomically checks-and-sets an in-process "currently
// resuming" flag for taskID. It returns false if the task is already being
// resumed by a goroutine in this process (the caller must back off), true if
// it successfully claimed the flag (the caller now owns calling
// unmarkResuming when done).
func (c *Coordinator) tryMarkResuming(taskID string) bool {
	c.resumingMu.Lock()
	defer c.resumingMu.Unlock()
	if c.resuming[taskID] {
		return false
	}
	c.resuming[taskID] = true
	return true
}

// unmarkResuming releases the in-process "currently resuming" flag for
// taskID. Must be called exactly once for every tryMarkResuming that
// returned true, on every exit path.
func (c *Coordinator) unmarkResuming(taskID string) {
	c.resumingMu.Lock()
	defer c.resumingMu.Unlock()
	delete(c.resuming, taskID)
}

// Heartbeat dispatches as many pending tasks as there are free worker slots,
// each on its own goroutine, then returns immediately. A slow or suspended task
// no longer blocks others. The returned Task is always zero-valued now (work is
// async); the bool reports whether at least one task was dispatched this tick.
func (c *Coordinator) Heartbeat(ctx context.Context) (domain.Task, bool, error) {
	if err := c.resumeScan(ctx); err != nil {
		return domain.Task{}, false, fmt.Errorf("resume scan: %w", err)
	}
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
				// transitioned the task to Failed on error; record why, so a
				// failed run is diagnosable rather than vanishing.
				c.reportRunFailure(ctx, t.ID, err)
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
			if _, err := c.locks.Unlock(ctx, taskToRun.ID, c.agent.ID); err != nil {
				return domain.Task{}, false, fmt.Errorf("release lock on suspend for task %s: %w", taskToRun.ID, err)
			}
			return c.currentTask(ctx, taskToRun.ID)
		}
		_ = c.scheduler.Transition(ctx, taskToRun.ID, domain.TaskFailed)
		return domain.Task{}, false, fmt.Errorf("run task: %w", err)
	}
	return c.afterRun(ctx, taskToRun, runnerAgent, run)
}

// afterRun finishes a task after its runner has produced a result: evaluate the
// trace for a hard loop, gate through quality review, and land the task in a
// terminal (or suspended, for hard-loop human review) state. It is the shared
// tail for both a fresh dispatch (runAssigned) and a resumed one (runResume).
func (c *Coordinator) afterRun(ctx context.Context, taskToRun domain.Task, runnerAgent domain.Agent, run domain.TaskRun) (domain.Task, bool, error) {
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

// resumeScan dispatches suspended tasks whose human decision has landed (they
// were flipped to Running by ApprovalCoordinator.Decide) and that carry a
// persisted checkpoint. A Running task without a checkpoint (mid fresh run,
// never yet suspended) is skipped, ruling out double-dispatch of a
// still-fresh-running task.
//
// Double-dispatch of the RESUME path itself is guarded two ways: the
// in-process resuming set (tryMarkResuming), claimed BEFORE TryLock and held
// for the whole runResume call regardless of lock TTL, and TryLock itself,
// which guards cross-process/restart races. The in-process set is required
// because TryLock's lease is a fixed duration that is never renewed — a
// runResume that outlives the lease would otherwise let a later tick's
// TryLock succeed again on the same still-Running, still-checkpointed task
// and start a second concurrent runResume. Called from Heartbeat each tick,
// before the pending-dispatch loop.
func (c *Coordinator) resumeScan(ctx context.Context) error {
	if c.checkpoints == nil {
		return nil
	}
	tasks, err := c.scheduler.List(ctx)
	if err != nil {
		return fmt.Errorf("list tasks for resume scan: %w", err)
	}
	for _, t := range tasks {
		if t.Status != domain.TaskRunning {
			continue
		}
		_, hasCP, err := c.checkpoints.Load(sessionKeyForTask(t), t.WorkingDir)
		if err != nil {
			return fmt.Errorf("load checkpoint for resume of task %s: %w", t.ID, err)
		}
		if !hasCP {
			continue // fresh Running task mid-flight, not a resume candidate
		}
		select {
		case c.sem <- struct{}{}:
		default:
			return nil // no worker slots; try next tick
		}
		if !c.tryMarkResuming(t.ID) {
			// Already being resumed by a goroutine in this process (its lock
			// lease may have expired while the run is still in flight — the
			// lease alone would let TryLock below succeed again). Skip; the
			// in-flight resume owns finishing this task.
			<-c.sem
			continue
		}
		locked, err := c.locks.TryLock(ctx, t.ID, c.agent.ID, c.lockTTL)
		if err != nil {
			c.unmarkResuming(t.ID)
			<-c.sem
			return fmt.Errorf("lock task %s for resume: %w", t.ID, err)
		}
		if !locked {
			c.unmarkResuming(t.ID)
			<-c.sem
			continue // an active worker already holds it
		}
		c.wg.Add(1)
		go func(rt domain.Task) {
			defer c.wg.Done()
			defer func() { <-c.sem }()
			defer c.unmarkResuming(rt.ID)
			if _, _, err := c.runResume(ctx, rt); err != nil {
				// Goroutine top-level: never swallow. runResume already transitioned
				// the task to Failed (or re-suspended it) on error; record the reason
				// so a failed resume is diagnosable rather than vanishing.
				_ = c.publishLearning(ctx, c.agent.ID, rt.ID, evolution.SignalFailure, "task_resume_error", true)
			}
		}(t)
	}
	return nil
}

// runResume runs a task that is already Running and lock-held (claimed by
// resumeScan) from its checkpoint. Unlike runAssigned it skips the
// Pending→Running transition (the task is already Running) and re-enters the
// runner, which auto-resumes from the checkpoint. On ErrSuspended it
// re-suspends (another undecided call surfaced) and releases the lock so a
// later decision can reclaim it; otherwise it finalises via afterRun.
func (c *Coordinator) runResume(ctx context.Context, t domain.Task) (domain.Task, bool, error) {
	runnerAgent := c.agent
	runner := c.runtime
	if c.taskRunnerResolver != nil {
		resolvedAgent, resolvedRunner, resolved, err := c.taskRunnerResolver.ResolveTaskRunner(ctx, t)
		if err != nil {
			_ = c.scheduler.Transition(ctx, t.ID, domain.TaskFailed)
			return domain.Task{}, false, fmt.Errorf("resolve runner for resume of task %s: %w", t.ID, err)
		}
		if resolved {
			if resolvedRunner == nil {
				_ = c.scheduler.Transition(ctx, t.ID, domain.TaskFailed)
				return domain.Task{}, false, fmt.Errorf("resolve runner for resume of task %s: runner is nil", t.ID)
			}
			runnerAgent, runner = resolvedAgent, resolvedRunner
		}
	}
	if runner == nil {
		_ = c.scheduler.Transition(ctx, t.ID, domain.TaskFailed)
		return domain.Task{}, false, fmt.Errorf("resume task %s: runtime is nil", t.ID)
	}
	run, err := runner.RunTask(ctx, runnerAgent, t)
	if err != nil {
		if errors.Is(err, ErrSuspended) {
			if txErr := c.scheduler.Transition(ctx, t.ID, domain.TaskSuspended); txErr != nil {
				return domain.Task{}, false, fmt.Errorf("re-suspend task %s: %w", t.ID, txErr)
			}
			if _, err := c.locks.Unlock(ctx, t.ID, c.agent.ID); err != nil {
				return domain.Task{}, false, fmt.Errorf("release lock on re-suspend for task %s: %w", t.ID, err)
			}
			return c.currentTask(ctx, t.ID)
		}
		_ = c.scheduler.Transition(ctx, t.ID, domain.TaskFailed)
		return domain.Task{}, false, fmt.Errorf("resume run task %s: %w", t.ID, err)
	}
	return c.afterRun(ctx, t, runnerAgent, run)
}

// RecoverSuspended re-registers every task named by checkpoints into the
// scheduler in TaskSuspended state, so suspends survive a process restart and
// remain visible/decidable. It does not resume them — resume is driven by a
// Suspended→Running decision. checkpoints is caller-assembled: a caller
// scanning multiple session-state bases (workspace root plus every
// working_dir base in use — see distinctSessionBases in the cli package)
// unions each base's ListSuspendedIn result before calling this, so a task
// suspended under a working_dir-bound session is recovered exactly like one
// suspended under the workspace root. Each recovered task carries
// cp.WorkingDir forward so a later resumeScan resolves the same session base
// via checkpoints.Load(key, task.WorkingDir) instead of silently defaulting
// back to the workspace root. Returns the number of tasks recovered. A task
// that is already present in the scheduler is skipped (idempotent re-scan).
func (c *Coordinator) RecoverSuspended(ctx context.Context, checkpoints []sessionstate.Checkpoint) (int, error) {
	recovered := 0
	for _, cp := range checkpoints {
		if _, ok, err := c.scheduler.Get(ctx, cp.TaskID); err != nil {
			return recovered, fmt.Errorf("check task %s presence: %w", cp.TaskID, err)
		} else if ok {
			continue
		}
		if err := c.scheduler.Add(ctx, domain.Task{
			ID:         cp.TaskID,
			AgentID:    cp.AgentID,
			SessionID:  cp.SessionKey,
			Status:     domain.TaskSuspended,
			Mode:       cp.Mode,
			WorkingDir: cp.WorkingDir,
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

// reportRunFailure records why a dispatched task run failed. It is called from
// the per-task goroutine's top level, where there is no caller left to return
// an error to, so it must not propagate — it reports on every channel it has
// and never drops the cause silently.
//
// Three channels, deliberately:
//   - the structured log, carrying the full wrapped error for operators;
//   - a RuntimeEventTaskFailed event, so clients watching the event stream (the
//     GUI's event panel) can show the cause instead of just "failed";
//   - the failure learning event, whose reason stays the plain enum value the
//     evolution pipeline expects.
//
// The event is published before the learning event so the cause is on the wire
// even if the learning publish then fails.
func (c *Coordinator) reportRunFailure(ctx context.Context, taskID string, cause error) {
	if c.logger != nil {
		c.logger.ErrorContext(ctx, "task run failed",
			"component", "coordinator",
			"task_id", taskID,
			"agent_id", c.agent.ID,
			"error", cause,
		)
	}
	if c.events != nil {
		if err := c.events.Publish(ctx, domain.RuntimeEvent{
			Type:      RuntimeEventTaskFailed,
			TaskID:    taskID,
			Message:   cause.Error(),
			CreatedAt: time.Now(),
		}); err != nil && c.logger != nil {
			c.logger.ErrorContext(ctx, "publish task failure event",
				"component", "coordinator",
				"task_id", taskID,
				"error", err,
			)
		}
	}
	if err := c.publishLearning(ctx, c.agent.ID, taskID, evolution.SignalFailure, "task_run_error", true); err != nil && c.logger != nil {
		c.logger.ErrorContext(ctx, "publish failure learning event",
			"component", "coordinator",
			"task_id", taskID,
			"error", err,
		)
	}
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
