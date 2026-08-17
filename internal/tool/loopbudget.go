package tool

import "context"

// LoopBudget is the per-task ceiling on how many times ONE tool may be called,
// shared by every writer that can start a tool call.
//
// A task has exactly one of these. The agent's own tool loop spends it, and so
// does anything the loop dispatches into that starts calls of its own — a
// plugin's call_tool host function. Giving a contributor its own counter would
// give it a channel around the task's total budget: the loop's runaway guard
// would see a quiet task while the plugin drove the same tool a thousand times.
//
// The names counted are domain.GuardedToolName(call), so every writer counts the
// same string for the same tool — the wrapped tool, not the protocol wrapper it
// was reached through.
type LoopBudget interface {
	// Record counts one call of name and reports the new count for that name
	// together with the limit in force, so the caller decides from the same two
	// numbers whichever side of the seam it is on.
	//
	// It is deliberately ONE call rather than a peek plus an increment: two
	// writers that read a count before incrementing it would each be told they
	// were the first past the post, and a budget whose whole purpose is to be
	// shared must not offer a check-then-act shape.
	//
	// The count is the running total INCLUDING this call, and the limit is how
	// many calls of one name the task allows. Recording is unconditional: a call
	// that the caller goes on to refuse still spent an attempt, exactly as the
	// agent's own loop records a call before it learns whether it will run.
	Record(name string) (count, limit int)
}

type loopBudgetKey struct{}

// WithLoopBudget attaches the task's shared tool-call budget to ctx, so
// everything reached under it that starts tool calls of its own spends the same
// allowance the agent's own loop spends.
//
// A nil budget is a programming error rather than "no budget": it would be
// indistinguishable on the way out from a ctx nobody attached a budget to, which
// is the state a plugin-initiated call must refuse. Say so here instead of
// letting it read as unlimited two frames later.
func WithLoopBudget(ctx context.Context, budget LoopBudget) context.Context {
	if budget == nil {
		panic("tool: WithLoopBudget requires a non-nil budget")
	}
	return context.WithValue(ctx, loopBudgetKey{}, budget)
}

// LoopBudgetFrom reports the shared tool-call budget attached to ctx.
//
// found is false when no budget was attached. That is NOT "this task has no
// limit": a caller that can start tool calls of its own must treat the absence
// as broken wiring and refuse the call, because the alternative — counting
// nothing — is exactly the bypass a shared budget exists to prevent.
func LoopBudgetFrom(ctx context.Context) (budget LoopBudget, found bool) {
	attached, ok := ctx.Value(loopBudgetKey{}).(LoopBudget)
	if !ok || attached == nil {
		return nil, false
	}
	return attached, true
}
