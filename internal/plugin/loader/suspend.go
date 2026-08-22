package loader

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The two runtime event types the dependency convergence publishes, in the
// same §8 style as plugin/loaded and plugin/unloaded. They are exported for
// the same reason those are: they are the contract an event subscriber selects
// on.
const (
	// RuntimeEventSuspended reports one plugin whose contributions were
	// withdrawn because something it requires cannot be resolved: the plugin,
	// its version, the unresolved tool names, whether the suspension cascaded
	// from another plugin's suspension, and the withdrawal's own failure if it
	// had one (error=).
	RuntimeEventSuspended = "plugin/suspended"

	// RuntimeEventResumed reports one plugin whose contributions were filed
	// again because everything it requires resolves once more: the plugin, its
	// version and the tools that came back.
	RuntimeEventResumed = "plugin/resumed"
)

// Whether a suspension followed from another plugin's suspension, as it
// appears in a RuntimeEventSuspended message.
//
// The two are worth telling apart in the stream: "the tool you depend on was
// never contributed by anybody" is an operator's missing entry, while "the
// plugin that contributes it was itself suspended" points one hop further up
// the chain, at a root cause the operator has to fix somewhere else.
const (
	cascadeYes = "yes"
	cascadeNo  = "no"
)

// convergeDependencies is the last step of one convergence: with the mounted
// set settled by Apply's three passes, it decides which mounted plugins can
// still work and moves each one to that state.
//
// It runs INSIDE the same ApplyAtBoundary as everything before it, and that is
// load-bearing. A plugin that is mounted by pass 3 and suspended here was never
// visible to a task in between — the whole convergence is one step from a
// task's point of view, so a task either sees the plugin advertising its tools
// or does not see it at all, never the instant in between.
//
// The order of the three phases is what makes a whole chain converge in a
// single Apply:
//
//  1. the graph is built from the mounted set and resolved (resolveStates);
//  2. everything that must go down goes down, dependents before providers;
//  3. everything that may come back comes back, providers before dependents —
//     a dependent resumed ahead of its provider would be brought up against a
//     tool that is still withdrawn.
//
// A graph that cannot be resolved at all — a dependency cycle, or two plugins
// claiming to provide one tool name — changes NOTHING. Every mounted plugin
// keeps the state it already had and the failure travels out in Apply's error:
// a bad manifest is no reason to tear down the plugins that are working.
//
// Each individual Suspend or Resume that fails is reported and does not abort
// the others, the same stance the passes before it take.
//
// It is called with l.mu held.
func (l *Loader) convergeDependencies(ctx context.Context) error {
	if len(l.instances) == 0 {
		return nil
	}

	names := make([]string, 0, len(l.instances))
	for name := range l.instances {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]depNode, 0, len(names))
	provided := make(map[string]bool)
	providerOf := make(map[string]string)
	for _, name := range names {
		inst := l.mounted(name)
		entries = append(entries, depNode{Name: name, Provides: inst.tools, Requires: inst.requires})
		for _, toolName := range inst.tools {
			provided[toolName] = true
			providerOf[toolName] = name
		}
	}

	// A name a mounted plugin contributes is the GRAPH's to decide, never the
	// registry's: a plugin that is about to be suspended in this very
	// convergence is still registered while the graph is being built, so
	// answering "resolvable" from the registry alone would tell a dependent
	// that a tool which is seconds away from being withdrawn is available —
	// and the cascade would stop one hop short every time.
	external := l.externalToolNames(names, provided)
	states, err := resolveStates(entries, func(toolName string) bool { return external[toolName] })
	if err != nil {
		return fmt.Errorf("resolve plugin dependencies: %w", err)
	}

	order := dependencyOrder(entries, providerOf)
	var errs []error

	// Down first, dependents before providers (the reverse of the activation
	// order), so a tool is never withdrawn from under a plugin this convergence
	// is about to withdraw anyway.
	for i := len(order) - 1; i >= 0; i-- {
		name := order[i]
		if states[name] != depSuspended {
			continue
		}
		inst := l.mounted(name)
		unresolved, cascaded := unresolvedRequires(inst, states, providerOf, external)
		inst.suspendedBy = unresolved
		if inst.plugin.Suspended() {
			// Already withdrawn by an earlier convergence, and Suspend refuses a
			// second call. Only the diagnosis above is refreshed: WHICH tool is
			// missing can change while the answer "suspended" does not.
			continue
		}
		if err := l.suspend(ctx, inst, unresolved, cascaded); err != nil {
			errs = append(errs, err)
		}
	}

	// Back up afterwards, providers before dependents, so every plugin that
	// comes back finds the tools it requires already registered.
	for _, name := range order {
		if states[name] != depActive {
			continue
		}
		inst := l.mounted(name)
		if !inst.plugin.Suspended() {
			inst.suspendedBy = nil
			continue
		}
		if err := l.resume(ctx, inst); err != nil {
			errs = append(errs, err)
			continue
		}
		inst.suspendedBy = nil
	}
	return errors.Join(errs...)
}

// mounted returns the instance registered under name, which every caller in
// this file has just read out of l.instances' own key set. A missing one is a
// bookkeeping bug in this package, not a state a deployment can produce, and
// so is an instance with no host.Plugin behind it: every path that puts an
// instance into l.instances (activate, restore) has one. Either would otherwise
// surface as a nil dereference several frames away from the mistake. It is
// called with l.mu held.
func (l *Loader) mounted(name string) *instance {
	inst, ok := l.instances[name]
	if !ok {
		panic(fmt.Sprintf("loader: plugin %q is in the mounted set but has no instance", name))
	}
	if inst.plugin == nil {
		panic(fmt.Sprintf("loader: mounted plugin %q has no host.Plugin; every activation and every restore "+
			"records one", name))
	}
	return inst
}

// externalToolNames is the set of tool names that resolve WITHOUT any mounted
// plugin: everything the registry holds, minus everything the mounted plugins
// contribute. Those are the host's own tools — the ones a plugin's call_tool
// reaches that no plugin's suspension can take away.
//
// The registry is read through the mounted instances because that is the
// registry a plugin's calls actually go out through (host.Spec.Registry, which
// Apply wires from Config.Deps): a name it does not hold cannot be called,
// whatever any catalog says about it. Every instance contributes its own view,
// so a deployment that handed two plugins different registries still has every
// reachable name counted. It is called with l.mu held.
func (l *Loader) externalToolNames(names []string, provided map[string]bool) map[string]bool {
	external := make(map[string]bool)
	for _, name := range names {
		for _, descriptor := range l.mounted(name).spec.Registry.Descriptors() {
			if provided[descriptor.Name] {
				continue
			}
			external[descriptor.Name] = true
		}
	}
	return external
}

// unresolvedRequires names the entries of inst.requires that nothing satisfies
// in the resolved graph, and reports whether at least one of them is provided
// by a plugin that is mounted but suspended — which is what makes a suspension
// a CASCADE rather than a plain missing dependency.
//
// It re-derives from the same three inputs resolveStates decided on (the
// external names, the provider of each tool, and the resolved states) rather
// than asking resolveStates for the detail, because the answer is a diagnosis
// for an operator — it is what Status reports in SuspendedBy — and not part of
// the decision itself.
func unresolvedRequires(inst *instance, states map[string]depState, providerOf map[string]string, external map[string]bool) (unresolved []string, cascaded bool) {
	for _, required := range inst.requires {
		if external[required] {
			continue
		}
		provider, provided := providerOf[required]
		if provided && states[provider] == depActive {
			continue
		}
		unresolved = append(unresolved, required)
		if provided {
			cascaded = true
		}
	}
	return unresolved, cascaded
}

// suspend withdraws one plugin's contributions, records why, and publishes
// plugin/suspended. It is called with l.mu held.
//
// The event goes out whether or not the withdrawal reported a failure, for the
// same reason unload's does: host.Plugin.Suspend disposes the contribution
// owner and flips the state whatever its disposers return, so the plugin IS
// suspended either way and an operator has to see it.
func (l *Loader) suspend(ctx context.Context, inst *instance, unresolved []string, cascaded bool) error {
	suspendErr := inst.plugin.Suspend(ctx)
	if suspendErr != nil {
		suspendErr = fmt.Errorf("suspend plugin %q (owner %s, unresolved %v): %w",
			inst.name, inst.owner, unresolved, suspendErr)
		inst.lastError = suspendErr.Error()
		l.logger.Error("plugin suspended with failures",
			"plugin", inst.name, "version", inst.version, "owner", string(inst.owner),
			"unresolved", unresolved, "cascade", cascaded, "error", suspendErr)
	} else {
		l.logger.Info("plugin suspended",
			"plugin", inst.name, "version", inst.version, "owner", string(inst.owner),
			"unresolved", unresolved, "cascade", cascaded)
	}
	l.publish(ctx, RuntimeEventSuspended, formatSuspendedMessage(inst, unresolved, cascaded, suspendErr))
	return suspendErr
}

// resume files one plugin's contributions again and publishes plugin/resumed.
// It is called with l.mu held.
//
// The pre-flight is the ORDER's own check, and it is not redundant with the
// graph: convergeDependencies resumes providers before dependents precisely so
// that every requirement is registered by the time its dependent comes back,
// and this is what holds that promise against the registry itself rather than
// against the plan. Resuming a plugin whose requirement is still withdrawn
// would put exactly what suspension exists to prevent back in front of the
// model — a tool whose every call must fail — so it is refused, naming the
// names, and the plugin stays suspended.
//
// No event is published for a resume that did not happen: unlike a suspension,
// a failed resume changes nothing at all (host.Plugin.Resume checks before it
// contributes), so a plugin/resumed would be reporting a state the plugin is
// not in.
func (l *Loader) resume(ctx context.Context, inst *instance) error {
	if missing := inst.unregisteredRequires(); len(missing) > 0 {
		unmet := fmt.Errorf("resume plugin %q (owner %s): required tool(s) %v are not registered; "+
			"a plugin may only come back once everything it calls into is available",
			inst.name, inst.owner, missing)
		inst.lastError = unmet.Error()
		l.logger.Error("plugin resume refused",
			"plugin", inst.name, "version", inst.version, "owner", string(inst.owner),
			"unresolved", missing, "error", unmet)
		return unmet
	}
	if err := inst.plugin.Resume(ctx); err != nil {
		resumeErr := fmt.Errorf("resume plugin %q (owner %s): %w", inst.name, inst.owner, err)
		inst.lastError = resumeErr.Error()
		l.logger.Error("plugin resume failed",
			"plugin", inst.name, "version", inst.version, "owner", string(inst.owner), "error", resumeErr)
		return resumeErr
	}
	l.logger.Info("plugin resumed",
		"plugin", inst.name, "version", inst.version, "owner", string(inst.owner), "tools", inst.tools)
	l.publish(ctx, RuntimeEventResumed, formatResumedMessage(inst))
	return nil
}

// unregisteredRequires names the tools this instance requires that its own
// registry does not currently hold, in the order they were declared. It is the
// live counterpart of the dependency graph's answer: the graph decides from a
// snapshot taken before anything moved, this reads the registry as it stands
// right now.
func (inst *instance) unregisteredRequires() []string {
	if len(inst.requires) == 0 {
		return nil
	}
	registered := make(map[string]bool)
	for _, descriptor := range inst.spec.Registry.Descriptors() {
		registered[descriptor.Name] = true
	}
	var missing []string
	for _, required := range inst.requires {
		if !registered[required] {
			missing = append(missing, required)
		}
	}
	return missing
}

// dependencyOrder returns every entry's name with providers ahead of the
// plugins that require them, breaking ties by entries' own (sorted) order so a
// convergence is reproducible.
//
// A cycle here is a programming error, not operator data: resolveStates refuses
// a cyclic graph before this is ever called, and convergeDependencies returns
// on that error without touching anything. So a cycle reaching this traversal
// means the two disagree about what a cycle is, and a traversal that quietly
// picked an order anyway would emit a resume sequence nobody can justify.
func dependencyOrder(entries []depNode, providerOf map[string]string) []string {
	byName := make(map[string]depNode, len(entries))
	for _, n := range entries {
		byName[n.Name] = n
	}

	placed := make(map[string]bool, len(entries))
	onPath := make(map[string]bool, len(entries))
	order := make([]string, 0, len(entries))

	var visit func(name string)
	visit = func(name string) {
		if placed[name] {
			return
		}
		if onPath[name] {
			panic(fmt.Sprintf("loader: dependency cycle at plugin %q while ordering resumes; "+
				"resolveStates must have refused this graph before it got here", name))
		}
		onPath[name] = true
		for _, required := range byName[name].Requires {
			if provider, ok := providerOf[required]; ok {
				visit(provider)
			}
		}
		onPath[name] = false
		placed[name] = true
		order = append(order, name)
	}
	for _, n := range entries {
		visit(n.Name)
	}
	return order
}

// formatSuspendedMessage renders a RuntimeEventSuspended payload. suspendErr is
// the withdrawal's own failure, or nil; the error= field is present either way
// (empty when the withdrawal was clean) so the payload keeps one shape a
// consumer can parse, exactly like plugin/unloaded's.
func formatSuspendedMessage(inst *instance, unresolved []string, cascaded bool, suspendErr error) string {
	cascade := cascadeNo
	if cascaded {
		cascade = cascadeYes
	}
	text := ""
	if suspendErr != nil {
		text = strings.ReplaceAll(suspendErr.Error(), "\n", "; ")
	}
	return fmt.Sprintf("plugin=%s version=%s unresolved=[%s] cascade=%s error=%s",
		inst.name, inst.version, strings.Join(unresolved, " "), cascade, text)
}

// formatResumedMessage renders a RuntimeEventResumed payload. It names the
// tools that came back, because that is what changed for the model.
func formatResumedMessage(inst *instance) string {
	return fmt.Sprintf("plugin=%s version=%s tools=[%s]",
		inst.name, inst.version, strings.Join(inst.tools, " "))
}
