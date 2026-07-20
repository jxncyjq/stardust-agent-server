package security

type Action string

const (
	ActionReadAudit    Action = "read_audit"
	ActionReadQuality  Action = "read_quality"
	ActionReadTask     Action = "read_task"
	ActionReadWorkflow Action = "read_workflow"
)

type Resource string

const (
	ResourceAudit    Resource = "audit"
	ResourceQuality  Resource = "quality"
	ResourceTask     Resource = "task"
	ResourceWorkflow Resource = "workflow"
)

// Policy decides RBAC and tenant access for a Principal built from request
// headers. Its RequireIdentity field selects between two explicitly contracted
// deployment shapes; see NewPolicy.
type Policy struct {
	// RequireIdentity selects whether callers must present an identity.
	//
	// When false (the default), a missing identity is a contractually optional
	// state rather than an error: an empty Principal.Role is treated as "admin"
	// and an empty Principal.CompanyID matches every company. This is the
	// single-machine / local deployment shape, where the agent serves only its
	// own operator through the loopback listener and no client sends X-Role or
	// X-Company-ID. The permissive mode is declared here — and announced by the
	// server at assembly time — precisely so it is a stated contract and not a
	// silent fallback.
	//
	// When true, an absent identity is rejected: an empty Role falls through to
	// the deny branch of Allows, and an empty CompanyID makes CanAccessCompany
	// return false. Callers then receive the same 403 + audit event as any other
	// denied request. Enable it for multi-tenant or network-exposed deployments
	// via server.require_identity (LEGION_AGENT_REQUIRE_IDENTITY).
	//
	// It is not a blanket authentication switch: it reaches only the handlers
	// that consult a Policy, and Role/CompanyID are caller-asserted headers that
	// bound anything only behind a gateway which injects them and strips the
	// client's own values. See server.Config.RequireIdentity for the exact
	// endpoint scope.
	RequireIdentity bool
}

// NewPolicy returns a Policy with the given identity requirement.
// Pass false for the permissive single-machine contract and true to require
// callers to present X-Role and X-Company-ID; see Policy.RequireIdentity.
func NewPolicy(requireIdentity bool) Policy {
	return Policy{RequireIdentity: requireIdentity}
}

// Allows reports whether principal may perform action on resource.
// An empty role is admin-equivalent only while Policy.RequireIdentity is false.
func (p Policy) Allows(principal Principal, action Action, resource Resource) bool {
	role := principal.Role
	if role == "" && !p.RequireIdentity {
		role = "admin"
	}
	switch role {
	case "admin":
		return true
	case "operator":
		return action == ActionReadAudit ||
			action == ActionReadQuality ||
			action == ActionReadTask ||
			action == ActionReadWorkflow
	case "viewer":
		return resource != ResourceAudit &&
			(action == ActionReadQuality ||
				action == ActionReadTask ||
				action == ActionReadWorkflow)
	default:
		return false
	}
}
