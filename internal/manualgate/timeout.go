package manualgate

import (
	"context"
	"errors"
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
//
// basesFn enumerates every session-state base to scan (the workspace root plus
// every working_dir base currently in use — see distinctSessionBases in the
// cli package); the sweep unions ListPendingIn(base) across all of them so a
// ticket filed under a working_dir-bound session is not silently skipped just
// because it is not under the workspace root. basesFn erroring aborts the
// sweep (fail-loud) rather than scanning a partial base set.
func NewTimeoutSweepJob(store *approval.ToolGateStore, dec *ApprovalCoordinator, ttl time.Duration, now func() time.Time, logger *slog.Logger, basesFn func(context.Context) ([]string, error)) func(context.Context) error {
	return func(ctx context.Context) error {
		if ttl <= 0 {
			return nil
		}
		bases, err := basesFn(ctx)
		if err != nil {
			return fmt.Errorf("enumerate session bases for timeout sweep: %w", err)
		}
		var pending []approval.ToolApproval
		for _, base := range bases {
			p, err := store.ListPendingIn(base)
			if err != nil {
				return fmt.Errorf("list pending approvals in base %q for timeout sweep: %w", base, err)
			}
			pending = append(pending, p...)
		}
		for _, rec := range pending {
			if now().Sub(rec.CreatedAt) <= ttl {
				continue
			}
			if _, err := dec.Decide(ctx, rec.TaskID, rec.TicketID, approval.ApprovalDenied); err != nil {
				// Benign race: a human (or another sweep pass) decided this
				// ticket between ListPending above and this Decide. The winning
				// decision is authoritative and the task resumes correctly, so
				// this is the intended outcome, not a fault — skip the ticket
				// instead of bubbling it up as a background-scheduler Error. Only
				// a genuinely unexpected failure still fails loud.
				if errors.Is(err, approval.ErrTicketAlreadyDecided) {
					continue
				}
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
