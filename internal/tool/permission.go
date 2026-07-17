package tool

import "github.com/stardust/legion-agent/internal/domain"

type PermissionEnforcerFunc func(domain.Agent, domain.ToolCall) error

func (f PermissionEnforcerFunc) Check(agent domain.Agent, call domain.ToolCall) error {
	return f(agent, call)
}

type RolePermissionOverride struct {
	Role     string
	ToolName string
	Allow    bool
}

type BatchRolePermissionEnforcer struct {
	allowed   map[string]bool
	overrides map[string]bool
}

func NewBatchRolePermissionEnforcer(allowed map[string]bool, overrides []RolePermissionOverride) BatchRolePermissionEnforcer {
	copiedAllowed := make(map[string]bool, len(allowed))
	for key, value := range allowed {
		copiedAllowed[key] = value
	}
	copiedOverrides := make(map[string]bool, len(overrides))
	for _, override := range overrides {
		copiedOverrides[permissionKey(override.Role, override.ToolName)] = override.Allow
	}
	return BatchRolePermissionEnforcer{
		allowed:   copiedAllowed,
		overrides: copiedOverrides,
	}
}

func (e BatchRolePermissionEnforcer) Check(agent domain.Agent, call domain.ToolCall) error {
	if e.allowedFor(agent.Role, call.Name) {
		return nil
	}
	return ErrPermissionDenied
}

func (e BatchRolePermissionEnforcer) CheckBatch(agent domain.Agent, calls []domain.ToolCall) []error {
	errs := make([]error, len(calls))
	for i, call := range calls {
		errs[i] = e.Check(agent, call)
	}
	return errs
}

func (e BatchRolePermissionEnforcer) allowedFor(role, toolName string) bool {
	key := permissionKey(role, toolName)
	if allow, ok := e.overrides[key]; ok {
		return allow
	}
	if allow, ok := e.allowed[key]; ok {
		return allow
	}
	developerKey := permissionKey("developer", toolName)
	if role != "developer" {
		return e.allowed[developerKey]
	}
	return false
}

func permissionKey(role, toolName string) string {
	return role + ":" + toolName
}
