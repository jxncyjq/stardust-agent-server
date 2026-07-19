package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// GatewayRunner wires adapters, session binding, task submission, and outbound
// delivery into the running gateway. It owns three concurrent loops: each
// adapter's ingress, an inbound worker, and the poll-driven delivery loop.
type GatewayRunner struct {
	cfg      GatewayConfig
	core     CoreClient
	binder   SessionBinder
	router   *DeliveryRouter
	tracker  *DeliveryTracker
	adapters []ChannelAdapter
	logger   *slog.Logger
	seq      uint64
	seqMu    sync.Mutex
	now      func() time.Time
}

// NewGatewayRunner assembles a runner from its collaborators.
func NewGatewayRunner(cfg GatewayConfig, core CoreClient, binder SessionBinder, router *DeliveryRouter, tracker *DeliveryTracker, adapters []ChannelAdapter, logger *slog.Logger) *GatewayRunner {
	if logger == nil {
		logger = slog.Default()
	}
	return &GatewayRunner{cfg: cfg, core: core, binder: binder, router: router, tracker: tracker, adapters: adapters, logger: logger, now: time.Now}
}

// deliveryRetries returns the configured maximum delivery attempts, defaulting
// to 3 when unset or invalid.
func (g *GatewayRunner) deliveryRetries() int {
	retries := g.cfg.Delivery.Retries
	if retries <= 0 {
		retries = 3
	}
	return retries
}

// deliveryBackoff returns the configured spacing between delivery retries,
// defaulting to 500ms when unset or invalid.
func (g *GatewayRunner) deliveryBackoff() time.Duration {
	backoff := time.Duration(g.cfg.Delivery.BackoffMS) * time.Millisecond
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	return backoff
}

// Run starts every adapter's ingress, an inbound worker, and the delivery loop,
// blocking until ctx is cancelled. A fatal error from any loop cancels the rest
// and is returned wrapped.
func (g *GatewayRunner) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	inbound := make(chan InboundMessage, 64)

	var wg sync.WaitGroup
	errCh := make(chan error, len(g.adapters)+2)

	for _, adapter := range g.adapters {
		wg.Add(1)
		go func(a ChannelAdapter) {
			defer wg.Done()
			if err := a.Start(ctx, inbound); err != nil {
				errCh <- fmt.Errorf("adapter %q start: %w", a.Platform(), err)
				cancel()
			}
		}(adapter)
	}

	wg.Go(func() {
		g.inboundWorker(ctx, inbound)
	})

	wg.Go(func() {
		g.pollLoop(ctx)
	})

	<-ctx.Done()
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		return err
	}
	return nil
}

func (g *GatewayRunner) inboundWorker(ctx context.Context, inbound <-chan InboundMessage) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-inbound:
			if err := g.HandleInbound(ctx, msg); err != nil {
				g.logger.Error("handle inbound message",
					"platform", msg.Platform, "chat", HashID(msg.ChatID), "err", err)
			}
		}
	}
}

// HandleInbound turns one inbound message into a Legion task: resolve-or-create
// the session for its chat (Legion sees only the hashed id), submit the task,
// and track the delivery target keyed by the minted task id.
func (g *GatewayRunner) HandleInbound(ctx context.Context, msg InboundMessage) error {
	key := msg.Platform + ":" + msg.ChatID
	// logKey never carries the raw chat id — only Legion/logs must ever see the
	// hashed form, per the gateway's PII rule. key (raw) stays scoped to the
	// binder calls below, which need the real id to resolve/persist bindings.
	logKey := msg.Platform + ":" + HashID(msg.ChatID)
	sessionID, _, ok, err := g.binder.Resolve(ctx, key)
	if err != nil {
		return fmt.Errorf("resolve binding %q: %w", logKey, err)
	}
	if !ok {
		sessionID, err = g.core.EnsureSession(ctx, SessionReq{
			CompanyID: g.cfg.Identity.CompanyID,
			AgentID:   g.cfg.Identity.AgentID,
			Project:   msg.Platform,
			Title:     msg.Platform + ":" + HashID(msg.ChatID),
		})
		if err != nil {
			return fmt.Errorf("ensure session for %q: %w", logKey, err)
		}
		if err := g.binder.Bind(ctx, key, sessionID, msg.ChatID); err != nil {
			return fmt.Errorf("bind session for %q: %w", logKey, err)
		}
	}
	taskID := g.mintTaskID(msg.Platform, msg.ChatID)
	if _, err := g.core.SubmitTask(ctx, TaskReq{
		ID:        taskID,
		Input:     msg.Text,
		CompanyID: g.cfg.Identity.CompanyID,
		AgentID:   g.cfg.Identity.AgentID,
		SessionID: sessionID,
		Images:    msg.Images,
	}); err != nil {
		return fmt.Errorf("submit task for %q: %w", logKey, err)
	}
	g.tracker.Track(taskID, DeliveryTarget{Platform: msg.Platform, ChatID: msg.ChatID})
	return nil
}

// pollInterval bounds how often the gateway checks its in-flight tasks for
// completion. Small enough for responsive replies, large enough to avoid
// hammering the core.
const pollInterval = 2 * time.Second

// pollLoop delivers completed tasks on a fixed interval until ctx is cancelled.
func (g *GatewayRunner) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := g.PollOnce(ctx); err != nil {
				g.logger.Warn("delivery poll pass", "err", err)
			}
		}
	}
}

// PollOnce checks every in-flight task once. A task that has reached a terminal
// status with an answer is delivered to the tracked target: on success it is
// removed from the tracker; on failure it is retried on a later pass (bounded
// by cfg.Delivery.Retries, spaced by cfg.Delivery.BackoffMS) until either
// delivery succeeds or the retry budget is exhausted, at which point it is
// dropped and the loss is logged at Error. Non-terminal tasks and tasks still
// within their backoff window are left for the next pass. A per-task
// result-fetch error is logged at the loop boundary and does not abort the
// pass — the task is retried on the next tick.
func (g *GatewayRunner) PollOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := g.now()
	retries := g.deliveryRetries()
	backoff := g.deliveryBackoff()
	for _, taskID := range g.tracker.Pending() {
		target, attempts, nextAt, ok := g.tracker.Get(taskID)
		if !ok {
			continue // taken by a concurrent pass
		}
		if now.Before(nextAt) {
			continue // backoff window not elapsed yet
		}
		text, done, err := g.core.TaskResult(ctx, taskID)
		if err != nil {
			g.logger.Warn("fetch task result", "task", taskID, "err", err)
			continue
		}
		if !done {
			continue
		}
		if text == "" {
			g.tracker.Take(taskID) // terminal but no answer (e.g. failed) — nothing to deliver
			continue
		}
		if err := g.router.Route(ctx, target, text); err != nil {
			if attempts+1 >= retries {
				g.tracker.Take(taskID)
				g.logger.Error("deliver reply permanently failed",
					"platform", target.Platform, "chat", HashID(target.ChatID), "task", taskID, "attempts", attempts+1, "err", err)
			} else {
				g.tracker.MarkAttempt(taskID, now.Add(backoff))
				g.logger.Warn("deliver reply failed; will retry",
					"platform", target.Platform, "chat", HashID(target.ChatID), "task", taskID, "attempt", attempts+1, "err", err)
			}
			continue
		}
		g.tracker.Take(taskID) // delivered
	}
	return nil
}

// mintTaskID builds a process-unique task id from platform + hashed chat + a
// monotonic counter, so submitted tasks never collide and carry no raw id.
func (g *GatewayRunner) mintTaskID(platform, chatID string) string {
	g.seqMu.Lock()
	g.seq++
	seq := g.seq
	g.seqMu.Unlock()
	return fmt.Sprintf("%s-%s-%d", platform, HashID(chatID), seq)
}
