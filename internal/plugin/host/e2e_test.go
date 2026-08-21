package host

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// e2eAgent is the identity the acceptance tests call as: a model-shaped call
// carries the agent Registry.Execute evaluates policy and audit against.
func e2eAgent() domain.Agent {
	return domain.Agent{ID: "agent-e2e", CompanyID: "co-1", Role: "developer", Status: domain.AgentActive}
}

// registryHasTool reports whether the registry advertises a tool by that name.
// It asks Descriptors() — the list the model is actually offered — rather than
// probing Execute, because "the model is offered it" and "a call would resolve"
// are two different claims, and the lifecycle test makes both separately.
func registryHasTool(registry *tool.Registry, name string) bool {
	for _, descriptor := range registry.Descriptors() {
		if descriptor.Name == name {
			return true
		}
	}
	return false
}

// TestPluginLifecycleFromActivationToDisposal is the acceptance test for one
// .wasm going through the whole lifecycle at the seams a deployment uses:
//
//	Activate → the tool is in Registry.Descriptors() and gateable → a
//	model-shaped Registry.Execute succeeds → DisposeOwner → ErrToolNotFound, no
//	longer gateable, no longer advertised, nothing left in the ledger, the pool
//	drained and the guest module closed.
//
// Every "after" assertion is paired with its "before": the tool is proven
// absent from the registry and from the gateable catalog BEFORE activation, and
// proven working BEFORE disposal, so neither half can pass on a state that was
// already true without this plugin.
func TestPluginLifecycleFromActivationToDisposal(t *testing.T) {
	ctx, guestClosed := watchGuestClose(context.Background())
	// The plugin's only guest instance is the pooled one (MaxInstances is 1 and
	// the manifest read borrows from the pool), so teardown must close exactly
	// one — counted, because guestClosed cannot say WHICH module was closed.
	instanceCloses := countInstanceCloses(t)

	audit := adapter.NewMemoryAuditLog()
	registry := tool.NewRegistry(nil, nil, nil).WithAuditLog(audit)
	ledger := lifecycle.NewLedger()

	spec := fixtureSpec(t)
	spec.Registry = registry

	if registryHasTool(registry, fixtureProvidedTool) {
		t.Fatalf("%q is advertised before activation: the assertions below would pass on a state that has "+
			"nothing to do with this plugin", fixtureProvidedTool)
	}
	if toolauth.IsGateable(fixtureProvidedTool) {
		t.Fatalf("%q is already gateable before activation: an earlier test leaked its contribution, and the "+
			"gateable assertions below would be vacuous", fixtureProvidedTool)
	}

	plugin, err := Activate(ctx, ledger, testOwner, spec)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// Unconditional, because toolauth.Contribute writes process-global state: a
	// test that left its contribution behind would panic the next contributor of
	// the same name. Disposal is asserted below; this is the safety net for a
	// failure between here and there.
	t.Cleanup(func() { _ = ledger.DisposeOwner(testOwner) })

	// 1. The model is offered the tool, with the descriptor the DEPLOYMENT
	// claimed rather than anything the guest chose: a plugin must not be able to
	// describe itself into another group or a lower risk level.
	var advertised tool.Descriptor
	found := false
	for _, descriptor := range registry.Descriptors() {
		if descriptor.Name == fixtureProvidedTool {
			advertised = descriptor
			found = true
		}
	}
	if !found {
		t.Fatalf("Registry.Descriptors() does not advertise %q after activation; got %v",
			fixtureProvidedTool, registry.Descriptors())
	}
	want := fixtureDescriptor()
	if advertised.Description != want.Description || advertised.Group != want.Group ||
		advertised.RiskLevel != want.RiskLevel || advertised.Timeout != want.Timeout {
		t.Errorf("advertised descriptor = %+v, want the deployment's claim %+v", advertised, want)
	}

	// 2. And the per-agent disabled_tools machinery can reach it. A tool that is
	// callable but not gateable is an authorization bypass, not a cosmetic gap.
	if !toolauth.IsGateable(fixtureProvidedTool) {
		t.Errorf("toolauth.IsGateable(%q) = false after activation: no agent config could disable this tool",
			fixtureProvidedTool)
	}

	// 3. A model-shaped call succeeds, and its answer proves the arguments
	// really travelled into the guest and the guest's answer really came back:
	// the fixture echoes "<tool>:<arguments as compact JSON>".
	result, err := registry.Execute(ctx, e2eAgent(), domain.ToolCall{
		ID:        "model-call-1",
		Name:      fixtureProvidedTool,
		Arguments: map[string]string{"text": "hi"},
	})
	if err != nil {
		t.Fatalf("Execute(%q): %v", fixtureProvidedTool, err)
	}
	if !result.Success {
		t.Errorf("result.Success = false (error %q), want true", result.Error)
	}
	if wantOutput := fixtureProvidedTool + `:{"text":"hi"}`; result.Output != wantOutput {
		t.Errorf("result.Output = %q, want %q", result.Output, wantOutput)
	}
	// The fixture always answers with the literal "guest-call-id" (see
	// testdata/README.md), so this pins that the HOST owns the correlation id.
	if result.CallID != "model-call-1" {
		t.Errorf("result.CallID = %q, want %q: the host, not the guest, owns the correlation id",
			result.CallID, "model-call-1")
	}

	// 4. The call is in the audit trail, attributed to whoever made it. This one
	// was model-initiated, so it must NOT read as plugin-initiated.
	events, err := audit.Events()
	if err != nil {
		t.Fatalf("read audit events: %v", err)
	}
	audited := false
	for _, ev := range events {
		if ev.Action != "tool_executed" || ev.RequestID != "model-call-1" {
			continue
		}
		audited = true
		if ev.Origin != tool.OriginAgent {
			t.Errorf("the model's call to the plugin tool was audited with origin %q, want %q",
				ev.Origin, tool.OriginAgent)
		}
	}
	if !audited {
		t.Errorf("no tool_executed audit row for the model's call; got %d events", len(events))
	}

	// 5. One DisposeOwner is the whole teardown.
	if err := ledger.DisposeOwner(testOwner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}

	if _, err := registry.Execute(context.Background(), e2eAgent(), domain.ToolCall{
		ID:   "model-call-2",
		Name: fixtureProvidedTool,
	}); !errors.Is(err, tool.ErrToolNotFound) {
		t.Errorf("Execute after disposal error = %v, want %v", err, tool.ErrToolNotFound)
	}
	if registryHasTool(registry, fixtureProvidedTool) {
		t.Errorf("Registry.Descriptors() still advertises %q after disposal: the model would keep being "+
			"offered a tool nothing can serve", fixtureProvidedTool)
	}
	if toolauth.IsGateable(fixtureProvidedTool) {
		t.Errorf("toolauth.IsGateable(%q) = true after disposal: the gateable entry outlived the plugin "+
			"that contributed it", fixtureProvidedTool)
	}
	if snapshot := ledger.Snapshot(); len(snapshot) != 0 {
		t.Errorf("ledger.Snapshot() = %v after disposal, want empty", snapshot)
	}
	// The pool is drained: a call arriving afterwards is refused by the pool
	// itself rather than reaching a closed wazero module.
	_, callErr := plugin.pool.call(context.Background(), opEcho, nil)
	if callErr == nil {
		t.Error("the plugin still answers a guest call after disposal, want an error: its pool was not drained")
	} else if !errors.Is(callErr, errPoolDraining) {
		t.Errorf("the drained pool refused with %v, want it to wrap %v", callErr, errPoolDraining)
	}
	if !guestClosed.Load() {
		t.Error("no guest module was closed by teardown: the disposers were dropped rather than run")
	}
	if got := instanceCloses.Load(); got != 1 {
		t.Errorf("closeInstance ran %d times across teardown, want exactly 1 (the plugin's one pooled instance)", got)
	}
}
