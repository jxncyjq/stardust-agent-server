package tool

import (
	"context"
	"errors"

	"github.com/stardust/legion-agent/internal/domain"
)

// ErrNoCaller reports that a tool needing the caller's identity was executed
// without one. Tools that enforce per-agent or per-tenant boundaries must fail
// with it rather than proceeding unscoped: an unfiltered query is precisely the
// escalation such a tool exists to prevent.
var ErrNoCaller = errors.New("tool call has no caller identity in context")

type callerContextKey struct{}

// WithCaller attaches the agent on whose behalf a tool call runs. Registry.Execute
// does this for every call, so handlers can rely on it being present.
//
// The identity travels in the context rather than in the Handler signature
// deliberately. Handler.Execute has ~17 production and ~60 test implementations;
// widening it for the handful of tools that need identity would spread a
// security fix across the whole codebase and bury it in mechanical churn. The
// trade-off is that a handler could forget to read it — which is why the tools
// that need identity call RequireCaller and refuse when it is missing, instead
// of silently continuing unscoped.
func WithCaller(ctx context.Context, agent domain.Agent) context.Context {
	return context.WithValue(ctx, callerContextKey{}, agent)
}

// CallerFrom returns the agent executing the current tool call.
func CallerFrom(ctx context.Context) (domain.Agent, bool) {
	agent, ok := ctx.Value(callerContextKey{}).(domain.Agent)
	return agent, ok
}

// RequireCaller returns the calling agent, or ErrNoCaller when the context
// carries none or carries one without an id. Use it in any tool whose result
// must be scoped to the caller.
func RequireCaller(ctx context.Context) (domain.Agent, error) {
	agent, ok := CallerFrom(ctx)
	if !ok || agent.ID == "" {
		return domain.Agent{}, ErrNoCaller
	}
	return agent, nil
}
