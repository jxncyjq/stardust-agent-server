package runtime

// DelegationAgents is the set of configured agents a delegation may name. It is
// the narrow half of the agent registry the delegation path needs: enough to
// decide whether a requested agent_id exists and to say which names are
// available when it does not.
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
}
