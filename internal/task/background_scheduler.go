package task

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
)

var ErrBackgroundSchedulerRunning = errors.New("background scheduler already running")

type BackgroundJob func(ctx context.Context) error

type GepRunner interface {
	Run(ctx context.Context, input evolution.ExtractionInput) (evolution.GepResult, error)
}

type BackgroundScheduler struct {
	running atomic.Bool
	mu      sync.Mutex
	jobs    []namedJob
	logger  *slog.Logger
}

// SetLogger sets the logger used to report background tick failures. Without it
// the scheduler falls back to slog.Default(). Job errors (e.g. a task failing
// in the coordinator heartbeat) must be recorded, not silently dropped.
func (s *BackgroundScheduler) SetLogger(logger *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logger = logger
}

func (s *BackgroundScheduler) reportError(err error) {
	s.mu.Lock()
	logger := s.logger
	s.mu.Unlock()
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("background scheduler tick failed", "error", err.Error())
}

type Clock func() time.Time

type namedJob struct {
	name string
	run  BackgroundJob
}

func NewBackgroundScheduler() *BackgroundScheduler {
	return &BackgroundScheduler{}
}

func NewLockReaperJob(store *LockStore, now Clock) BackgroundJob {
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context) error {
		_, err := store.ReapExpired(ctx, now())
		if err != nil {
			return fmt.Errorf("reap expired locks: %w", err)
		}
		return nil
	}
}

func NewGepFailureScanJob(events port.EventBus, runner GepRunner) BackgroundJob {
	processed := make(map[string]bool)
	var mu sync.Mutex
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if events == nil || runner == nil {
			return nil
		}
		published, err := events.Events()
		if err != nil {
			return fmt.Errorf("read runtime events for gep failure scan: %w", err)
		}
		for _, event := range published {
			learning, ok := evolution.ParseLearningRuntimeEvent(event)
			if !ok || !shouldRunGepForSignal(learning.Signal) {
				continue
			}
			key := learningEventKey(event)
			mu.Lock()
			if processed[key] {
				mu.Unlock()
				continue
			}
			processed[key] = true
			mu.Unlock()
			if _, err := runner.Run(ctx, extractionInputFromLearning(learning)); err != nil {
				return fmt.Errorf("run gep failure scan for %s: %w", learning.TaskID, err)
			}
		}
		return nil
	}
}

func (s *BackgroundScheduler) AddJob(name string, job BackgroundJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, namedJob{name: name, run: job})
}

func shouldRunGepForSignal(signal evolution.SignalKind) bool {
	switch signal {
	case evolution.SignalFailure, evolution.SignalHardLoopFailure, evolution.SignalBudgetExhausted:
		return true
	default:
		return false
	}
}

func extractionInputFromLearning(event evolution.LearningEvent) evolution.ExtractionInput {
	status := domain.TaskFailed
	eval := quality.EvalResult{Status: quality.EvalNormal}
	if event.Signal == evolution.SignalHardLoopFailure {
		status = domain.TaskSuspended
		eval = quality.EvalResult{
			Status: quality.EvalHardLoop,
			Reason: event.Reason,
		}
	}
	return evolution.ExtractionInput{
		AgentID: event.AgentID,
		Task: domain.Task{
			ID:      event.TaskID,
			AgentID: event.AgentID,
			Status:  status,
			Input:   event.TaskID + " " + event.Reason,
		},
		Eval:  eval,
		Cycle: int(event.PublishedAt.Unix()),
	}
}

func learningEventKey(event domain.RuntimeEvent) string {
	return event.TaskID + ":" + event.Message + ":" + event.CreatedAt.UTC().Format(time.RFC3339Nano)
}

func (s *BackgroundScheduler) RunOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.running.CompareAndSwap(false, true) {
		return ErrBackgroundSchedulerRunning
	}
	defer s.running.Store(false)
	for _, job := range s.snapshotJobs() {
		if err := job.run(ctx); err != nil {
			return fmt.Errorf("run background job %s: %w", job.name, err)
		}
	}
	return nil
}

func (s *BackgroundScheduler) Start(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
					s.reportError(err)
				}
			}
		}
	}()
	return done
}

func (s *BackgroundScheduler) snapshotJobs() []namedJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]namedJob(nil), s.jobs...)
}
