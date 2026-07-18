package manualgate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/stardust/legion-agent/internal/approval"
)

// NewTimeoutSweepJob returns a background job that denies every pending tool
// approval older than ttl (measured against now()), routing each through the
// ApprovalCoordinator so the owning task resumes down the deny branch. A denied-
// on-timeout ticket is a contract outcome (reject result to the model), not a
// silent drop — the job logs a warn per timeout. ttl<=0 disables the sweep.
func NewTimeoutSweepJob(store *approval.ToolGateStore, dec *ApprovalCoordinator, ttl time.Duration, now func() time.Time, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		if ttl <= 0 {
			return nil
		}
		pending, err := store.ListPending()
		if err != nil {
			return fmt.Errorf("list pending approvals for timeout sweep: %w", err)
		}
		for _, rec := range pending {
			if now().Sub(rec.CreatedAt) <= ttl {
				continue
			}
			if _, err := dec.Decide(ctx, rec.TaskID, rec.TicketID, approval.ApprovalDenied); err != nil {
				return fmt.Errorf("timeout-deny ticket %s: %w", rec.TicketID, err)
			}
			if logger != nil {
				logger.Warn("approval timed out, auto-denied",
					"task_id", rec.TaskID, "ticket_id", rec.TicketID, "tool", rec.ToolName)
			}
		}
		return nil
	}
}
