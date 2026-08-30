package host

import (
	"fmt"
	"strings"
)

// ServicePrefix marks a call_tool target as a named service rather than a tool:
// "service:issue-tracker/search" asks for the "search" capability of whoever
// currently provides "issue-tracker".
//
// It is a prefix on the TOOL NAME rather than a separate request field because
// call_tool's request shape is ABI v1 and already shipped; a guest that knows
// nothing about services keeps working unchanged, and one that does needs no
// new op.
const ServicePrefix = "service:"

// ServiceResolver answers "which tool is the <capability> of <service> right
// now?".
//
// Right now is the whole point: providers mount and unload while the process
// runs, so the answer is looked up per call rather than captured when a plugin
// was activated. The Loader implements it (it is what knows who holds a
// service name); the host only asks.
type ServiceResolver interface {
	ResolveService(service, capability string) (toolName string, err error)
}

// parseServiceTarget splits "service:<name>/<capability>" into its two halves.
//
// It reports ok=false for a plain tool name — that is the ordinary case, not
// an error — and an error for a target that announces itself as a service and
// then is not one: an empty service name, an empty capability, or no "/" at
// all. Guessing at any of those (treating it as a tool name, say) would send
// the guest an unrelated "tool not found" for what is actually a typo in a
// service reference.
func parseServiceTarget(target string) (service, capability string, ok bool, err error) {
	if !strings.HasPrefix(target, ServicePrefix) {
		return "", "", false, nil
	}
	rest := strings.TrimPrefix(target, ServicePrefix)
	service, capability, found := strings.Cut(rest, "/")
	if !found {
		return "", "", true, fmt.Errorf("service target %q has no capability; expected %s<service>/<capability>",
			target, ServicePrefix)
	}
	if strings.TrimSpace(service) == "" {
		return "", "", true, fmt.Errorf("service target %q names no service; expected %s<service>/<capability>",
			target, ServicePrefix)
	}
	if strings.TrimSpace(capability) == "" {
		return "", "", true, fmt.Errorf("service target %q names no capability; expected %s<service>/<capability>",
			target, ServicePrefix)
	}
	return service, capability, true, nil
}

// resolveServiceTarget turns a call_tool target into the tool name to dispatch.
//
// A plain tool name passes through untouched. A service target is resolved
// through deps.Services, and EVERY failure is an error rather than a fallback:
//
//   - a deployment with no resolver wired says so, instead of looking the
//     literal "service:x/y" up in the registry and reporting the unrelated
//     "tool not found" that would produce;
//   - an unprovided service, or a provider that does not expose the
//     capability, comes back naming what was asked for.
//
// The resolved name is what everything downstream sees: the shared per-task
// budget counts it, the registry dispatches it, the audit trail records it. A
// service name is a way to REACH a tool, never a thing that runs.
func resolveServiceTarget(deps Deps, target string) (string, error) {
	service, capability, isService, err := parseServiceTarget(target)
	if err != nil {
		return "", err
	}
	if !isService {
		return target, nil
	}
	if deps.Services == nil {
		return "", fmt.Errorf("call target %q names a service, but this deployment has no service resolver "+
			"wired; named services are not available here", target)
	}
	toolName, err := deps.Services.ResolveService(service, capability)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", target, err)
	}
	if toolName == "" {
		// A resolver that answers "" with no error is broken wiring: dispatching
		// an empty name would fail as an unrelated unknown tool.
		return "", fmt.Errorf("resolve %q: the resolver returned no tool name", target)
	}
	return toolName, nil
}
