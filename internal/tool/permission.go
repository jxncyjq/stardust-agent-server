package tool

import (
	"maps"

	"github.com/stardust/legion-agent/internal/domain"
)

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
	// dynamic answers "was this tool contributed at run time?" for names that
	// cannot be in a compile-time whitelist — today, the tools mounted
	// plugins contribute. Nil means there is no such source, and the enforcer
	// behaves exactly as it did before one existed.
	dynamic func(toolName string) bool
}

// WithDynamicTools returns a copy of e that also admits tools a run-time
// source reports as contributed.
//
// It is how a plugin's tool becomes callable at all: its name appears while
// the process runs, so no whitelist can carry it. It is deliberately NOT a
// bypass — the source is consulted on the same footing as the whitelist,
// AFTER any explicit per-agent override, so a disabled_tools entry naming a
// plugin tool still removes it.
func (e BatchRolePermissionEnforcer) WithDynamicTools(contributed func(toolName string) bool) BatchRolePermissionEnforcer {
	e.dynamic = contributed
	return e
}

func NewBatchRolePermissionEnforcer(allowed map[string]bool, overrides []RolePermissionOverride) BatchRolePermissionEnforcer {
	copiedAllowed := make(map[string]bool, len(allowed))
	maps.Copy(copiedAllowed, allowed)
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
		if e.allowed[developerKey] {
			return true
		}
		return e.contributed(toolName)
	}
	return e.contributed(toolName)
}

// contributed asks the dynamic source, if there is one. A nil source answers
// no, which is what makes a deployment with no plugins behave identically to
// one built before this existed.
func (e BatchRolePermissionEnforcer) contributed(toolName string) bool {
	return e.dynamic != nil && e.dynamic(toolName)
}

func permissionKey(role, toolName string) string {
	return role + ":" + toolName
}
