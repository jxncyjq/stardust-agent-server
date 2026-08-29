package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// A task's registry inherits the plugin registry so the model can reach what
// plugins contribute. It is NOT a Subset/Without view: those share the
// parent's policy, and this one must run a plugin's tool under the TASK's
// policy, guardrails and audit log.

func pluginSideRegistry(t *testing.T, ran *bool) *Registry {
	t.Helper()

	plugins := NewRegistry(
		NewStaticPolicy(DecisionAllow),
		PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		NoopGuardrails{},
	)
	plugins.RegisterDescriptor(Descriptor{Name: "jira_search", Description: "d", Group: "plugins", RiskLevel: "low"},
		HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			if ran != nil {
				*ran = true
			}
			return domain.ToolResult{Success: true, Output: "issues"}, nil
		}))
	return plugins
}

func taskRegistryInheriting(plugins *Registry, enforcer PermissionEnforcer, guards Guardrails) *Registry {
	task := NewRegistry(NewStaticPolicy(DecisionAllow), enforcer, guards)
	task.InheritFrom(plugins)
	return task
}

func allowAll() PermissionEnforcer {
	return PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil })
}

func pluginCall() domain.ToolCall { return domain.ToolCall{ID: "c1", Name: "jira_search"} }

func TestATaskRegistryExecutesAnInheritedPluginTool(t *testing.T) {
	t.Parallel()

	ran := false
	task := taskRegistryInheriting(pluginSideRegistry(t, &ran), allowAll(), NoopGuardrails{})

	result, err := task.Execute(context.Background(), developer(), pluginCall())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !ran || result.Output != "issues" {
		t.Errorf("handler ran = %t, result = %+v; want the plugin's tool to have run", ran, result)
	}
}

// TestAnInheritedToolIsVisibleToTheModel: reaching it is only half of it. A
// tool the model cannot SEE is a tool it will never call.
func TestAnInheritedToolIsVisibleToTheModel(t *testing.T) {
	t.Parallel()

	task := taskRegistryInheriting(pluginSideRegistry(t, nil), allowAll(), NoopGuardrails{})

	var names []string
	for _, descriptor := range task.Descriptors() {
		names = append(names, descriptor.Name)
	}
	if !containsName(names, "jira_search") {
		t.Errorf("descriptors = %v, want the inherited plugin tool", names)
	}
}

// TestUnmountingAPluginRemovesItFromEveryTaskRegistry: the inheritance is a
// REFERENCE, not a copy. A registry that kept answering for an unloaded
// plugin would be the exact defect the ledger exists to prevent.
func TestUnmountingAPluginRemovesItFromEveryTaskRegistry(t *testing.T) {
	t.Parallel()

	plugins := pluginSideRegistry(t, nil)
	task := taskRegistryInheriting(plugins, allowAll(), NoopGuardrails{})
	revoke := plugins.RegisterDescriptor(Descriptor{Name: "jira_comment", Group: "plugins", RiskLevel: "low"},
		HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{Success: true}, nil
		}))

	if _, err := task.Execute(context.Background(), developer(),
		domain.ToolCall{ID: "c1", Name: "jira_comment"}); err != nil {
		t.Fatalf("Execute before revocation: %v", err)
	}
	revoke()
	_, err := task.Execute(context.Background(), developer(), domain.ToolCall{ID: "c2", Name: "jira_comment"})
	if !errors.Is(err, ErrToolNotFound) {
		t.Errorf("Execute after revocation = %v, want ErrToolNotFound", err)
	}
}

// TestAnInheritedToolRunsUnderTheTasksOwnPolicy is the difference from a
// Subset view: the plugin registry allows everything, and the task's own
// policy still decides. A plugin's tool must not arrive with the plugin
// registry's permissions attached.
func TestAnInheritedToolRunsUnderTheTasksOwnPolicy(t *testing.T) {
	t.Parallel()

	ran := false
	plugins := pluginSideRegistry(t, &ran)
	task := NewRegistry(NewStaticPolicy(DecisionDeny), allowAll(), NoopGuardrails{})
	task.InheritFrom(plugins)

	if _, err := task.Execute(context.Background(), developer(), pluginCall()); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("Execute = %v, want the task's own policy to refuse", err)
	}
	if ran {
		t.Error("the plugin's tool ran despite the task's policy denying it")
	}
}

func TestAnInheritedToolRunsUnderTheTasksOwnGuardrails(t *testing.T) {
	t.Parallel()

	ran := false
	guardErr := errors.New("guardrail refused")
	task := taskRegistryInheriting(pluginSideRegistry(t, &ran), allowAll(), guardrailsFunc{before: guardErr})

	if _, err := task.Execute(context.Background(), developer(), pluginCall()); !errors.Is(err, guardErr) {
		t.Fatalf("Execute = %v, want the task's guardrail refusal", err)
	}
	if ran {
		t.Error("the plugin's tool ran past a guardrail that refused it")
	}
}

// TestAnInheritedToolIsStillSubjectToPermissions: inheriting is not
// authorizing. The enforcer decides, and a deployment that has not admitted
// the tool still refuses it.
func TestAnInheritedToolIsStillSubjectToPermissions(t *testing.T) {
	t.Parallel()

	task := taskRegistryInheriting(pluginSideRegistry(t, nil),
		NewRolePermissionEnforcer(map[string]bool{}), NoopGuardrails{})

	if _, err := task.Execute(context.Background(), developer(), pluginCall()); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("Execute = %v, want the enforcer to refuse an unadmitted tool", err)
	}
}

// TestABuiltinToolShadowsAnInheritedOneOfTheSameName: own registrations win.
// The direction matters — the reverse would let a plugin silently replace
// write_file.
func TestABuiltinToolShadowsAnInheritedOneOfTheSameName(t *testing.T) {
	t.Parallel()

	plugins := NewRegistry(NewStaticPolicy(DecisionAllow), allowAll(), NoopGuardrails{})
	plugins.RegisterDescriptor(Descriptor{Name: "write_file", Group: "plugins"},
		HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{Success: true, Output: "from the plugin"}, nil
		}))
	task := taskRegistryInheriting(plugins, allowAll(), NoopGuardrails{})
	task.RegisterDescriptor(Descriptor{Name: "write_file", Group: "builtin"},
		HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{Success: true, Output: "from the host"}, nil
		}))

	result, err := task.Execute(context.Background(), developer(), domain.ToolCall{ID: "c1", Name: "write_file"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Output != "from the host" {
		t.Errorf("output = %q, want the host's own tool to win", result.Output)
	}
}

// TestHasToolReportsWhatThisRegistryContributes backs the permission
// enforcer's dynamic source: "is this name one the plugins contributed?"
func TestHasToolReportsWhatThisRegistryContributes(t *testing.T) {
	t.Parallel()

	plugins := pluginSideRegistry(t, nil)
	if !plugins.HasTool("jira_search") {
		t.Error("HasTool(jira_search) = false, want true")
	}
	if plugins.HasTool("read_file") {
		t.Error("HasTool(read_file) = true, want false: nothing registered it here")
	}
}

// TestTheEnforcerAdmitsDynamicallyContributedTools: a plugin's tool name
// cannot be in a compile-time whitelist. The enforcer takes a source of names
// instead — and it is consulted on the same footing as the whitelist, so a
// per-agent disabled_tools list still removes the tool.
func TestTheEnforcerAdmitsDynamicallyContributedTools(t *testing.T) {
	t.Parallel()

	plugins := pluginSideRegistry(t, nil)
	enforcer := NewBatchRolePermissionEnforcer(map[string]bool{"developer:read_file": true}, nil).
		WithDynamicTools(plugins.HasTool)

	if err := enforcer.Check(developer(), pluginCall()); err != nil {
		t.Errorf("Check(jira_search) = %v, want it admitted: a plugin contributed it", err)
	}
	if err := enforcer.Check(developer(), domain.ToolCall{Name: "nothing_registered_this"}); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("Check(unknown) = %v, want a refusal", err)
	}
	// A role other than developer reaches a plugin tool the same way it
	// reaches a builtin one: through the developer fallback.
	if err := enforcer.Check(domain.Agent{ID: "a2", Role: "researcher"}, pluginCall()); err != nil {
		t.Errorf("Check(researcher, jira_search) = %v, want it admitted", err)
	}
}

// TestAnEnforcerWithNoDynamicSourceIsUnchanged: a deployment with no plugins
// must behave exactly as it did before this seam existed.
func TestAnEnforcerWithNoDynamicSourceIsUnchanged(t *testing.T) {
	t.Parallel()

	enforcer := NewBatchRolePermissionEnforcer(map[string]bool{"developer:read_file": true}, nil)

	if err := enforcer.Check(developer(), domain.ToolCall{Name: "read_file"}); err != nil {
		t.Errorf("Check(read_file) = %v, want nil", err)
	}
	if err := enforcer.Check(developer(), pluginCall()); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("Check(jira_search) = %v, want a refusal with no dynamic source", err)
	}
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// guardrailsFunc is a Guardrails whose Before fails with a fixed error.
type guardrailsFunc struct{ before error }

func (g guardrailsFunc) Before(context.Context, domain.ToolCall) error { return g.before }
func (g guardrailsFunc) After(context.Context, domain.ToolCall, domain.ToolResult) error {
	return nil
}
