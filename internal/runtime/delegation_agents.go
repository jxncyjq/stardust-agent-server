package runtime

import (
	"context"

	"github.com/stardust/legion-agent/internal/domain"
)

// DelegationContext is what the delegating side decides about a child, as
// opposed to what the named agent's own configuration decides. Depth and
// MaxSpawnDepth must reach the child unchanged: a child built at depth 0 would
// sit outside the spawn ceiling its parent is already inside, and would take a
// task-boundary token as an arriving task rather than as a child of one.
type DelegationContext struct {
	// Depth is the child's depth: the parent's depth plus one.
	Depth int
	// MaxSpawnDepth is the ceiling the parent runs under, carried down unchanged.
	MaxSpawnDepth int
	// Role is the DELEGATION role (roleLeaf or roleOrchestrator): whether this
	// child may delegate further. It is not the agent's tool-permission role,
	// which comes from that agent's own configuration. ResolveDelegate refuses
	// roleOrchestrator outright: a named agent's own runtime never registers
	// delegate_task, so canDelegate() reading true off it would be a grant with
	// nothing to act on.
	Role string
	// Toolsets is the tool-name narrowing the delegation request asked for,
	// empty when it asked for none. The names are the DELEGATING runtime's tool
	// names — validateSubTaskSpec checks them against that runtime's registry —
	// and a named agent runs on a registry of its own, so a resolver either maps
	// them onto that registry or refuses; narrowing to whatever happens to match
	// would hand the child a silently different tool set than was asked for.
	// ResolveDelegate refuses a non-empty Toolsets outright, the same way it
	// refuses roleOrchestrator above.
	Toolsets []string
}

// DelegationAgents is the set of configured agents a delegation may name. It is
// the narrow half of the agent registry the delegation path needs: enough to
// decide whether a requested agent_id exists, to say which names are available
// when it does not, and to build the child that runs as the named agent.
//
// A nil DelegationAgents means this deployment configured no agent directory.
// That is a legitimate state, not a fallback: delegation without agent_id keeps
// working. What must never happen is a request that names an agent being served
// as if it had not — see validateSubTaskSpec.
type DelegationAgents interface {
	// AgentNames returns the configured agent ids, sorted, for error messages.
	AgentNames() []string
	// HasAgent reports whether id names a configured agent.
	HasAgent(id string) bool
	// ResolveDelegate builds a child runtime for the named agent: that agent's
	// own configuration decides the tool-permission role, tool authorisation,
	// model profile and workspace, and dc decides everything the delegating
	// side owns. The returned domain.Agent is the identity the child runs its
	// task under.
	//
	// Every failure is returned, including an id this set does not recognise: a
	// caller that named an agent and got a runtime back must be able to trust
	// that the runtime is that agent's.
	ResolveDelegate(ctx context.Context, id string, dc DelegationContext) (domain.Agent, *Runtime, error)
}
