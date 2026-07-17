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

type RBACPolicy struct{}

func NewRBACPolicy() RBACPolicy {
	return RBACPolicy{}
}

func (RBACPolicy) Allows(principal Principal, action Action, resource Resource) bool {
	switch principal.Role {
	case "", "admin":
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
