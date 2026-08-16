package tool

import (
	"context"

	"github.com/stardust/legion-agent/internal/domain"
)

// OriginAgent is the origin of a tool call the agent made for itself. It is the
// default so an unmarked call is attributed to the agent rather than to an
// empty string that reads as "unknown" in the audit trail.
const OriginAgent = domain.OriginAgent

type callOriginKey struct{}

// WithCallOrigin marks every tool call made under ctx as initiated by origin —
// "delegate:depth-1", "plugin:jira". The audit trail records it so a forensic
// pass over a time window can tell whose calls it is reading; without it a
// contributor driving calls of its own is indistinguishable from the agent.
//
// An empty origin is a programming error: marking a call with nothing is worse
// than not marking it, because it overwrites the OriginAgent default with a
// blank.
func WithCallOrigin(ctx context.Context, origin string) context.Context {
	if origin == "" {
		panic("tool: WithCallOrigin requires a non-empty origin")
	}
	return context.WithValue(ctx, callOriginKey{}, origin)
}

// CallOriginFrom reports the origin marked on ctx, or OriginAgent when the call
// is unmarked.
func CallOriginFrom(ctx context.Context) string {
	if origin, ok := ctx.Value(callOriginKey{}).(string); ok && origin != "" {
		return origin
	}
	return OriginAgent
}
